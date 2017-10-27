// Copyright (C) 2017, Beijing Bochen Technology Co.,Ltd.  All rights reserved.
//
// This file is part of L0
//
// The L0 is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The L0 is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package validator

import (
	"bytes"
	"math/big"
	"sync"
	"time"

	"github.com/bocheninc/L0/components/crypto"
	"github.com/bocheninc/L0/components/log"
	"github.com/bocheninc/L0/components/utils/sortedlinkedlist"
	"github.com/bocheninc/L0/core/accounts"
	"github.com/bocheninc/L0/core/consensus"
	"github.com/bocheninc/L0/core/ledger"
	"github.com/bocheninc/L0/core/ledger/state"
	"github.com/bocheninc/L0/core/types"
)

type Validator interface {
	Start()
	ProcessTransaction(tx *types.Transaction) bool
	VerifyTxs(txs types.Transactions, primary bool) (bool, types.Transactions)
	UpdateAccount(tx *types.Transaction) bool
	RollBackAccount(tx *types.Transaction)
	RemoveTxsInVerification(txs types.Transactions)
	GetTransactionByHash(txHash crypto.Hash) (*types.Transaction, bool)
	GetBalance(addr accounts.Address) *state.Balance
}

type Verification struct {
	config             *Config
	ledger             *ledger.Ledger
	consenter          consensus.Consenter
	txpool             *sortedlinkedlist.SortedLinkedList
	requestBatchSignal chan int
	requestBatchTimer  *time.Timer
	blacklist          map[string]time.Time
	rwBlacklist        sync.RWMutex
	accounts           map[string]*state.Balance
	rwAccount          sync.RWMutex
	assets             map[uint32]*state.Asset
	inTxs              map[crypto.Hash]*types.Transaction
	rwInTxs            sync.RWMutex
	sync.RWMutex
}

func NewVerification(config *Config, ledger *ledger.Ledger, consenter consensus.Consenter) *Verification {
	return &Verification{
		config:             config,
		ledger:             ledger,
		consenter:          consenter,
		txpool:             sortedlinkedlist.NewSortedLinkedList(),
		requestBatchSignal: make(chan int),
		requestBatchTimer:  time.NewTimer(consenter.BatchTimeout()),
		blacklist:          make(map[string]time.Time),
		accounts:           make(map[string]*state.Balance),
		assets:             make(map[uint32]*state.Asset),
		inTxs:              make(map[crypto.Hash]*types.Transaction),
	}
}

func (v *Verification) Start() {
	log.Info("validator start ...")
	go v.processLoop()
}

func (v *Verification) makeRequestBatch() types.Transactions {
	var requestBatch types.Transactions
	var to string
	v.requestBatchTimer.Reset(v.consenter.BatchTimeout())
	v.txpool.IterElement(func(element sortedlinkedlist.IElement) bool {
		tx := element.(*types.Transaction)
		if to == "" {
			to = tx.ToChain()
		}
		if tx.ToChain() == to && len(requestBatch) < v.consenter.BatchSize() {
			requestBatch = append(requestBatch, tx)
		} else {
			return true
		}
		return false
	})

	return requestBatch
}

func (v *Verification) processLoop() {
	ticker := time.NewTicker(v.config.BlacklistDur)
	for {
		select {
		case <-ticker.C:
			v.rwBlacklist.Lock()
			for address, created := range v.blacklist {
				if created.Add(v.config.BlacklistDur).Before(time.Now()) {
					delete(v.blacklist, address)
				}
			}
			v.rwBlacklist.Unlock()
		case cnt := <-v.requestBatchSignal:
			if cnt > (v.config.TxPoolDelay + v.consenter.BatchSize()) {
				requestBatch := v.makeRequestBatch()
				log.Debugf("request Batch: %d ", len(requestBatch))
				v.consenter.ProcessBatch(requestBatch, v.consensusFailed)
			}
		case <-v.requestBatchTimer.C:
			if requestBatch := v.makeRequestBatch(); len(requestBatch) != 0 {
				log.Debugf("request Batch Timeout: %d ", len(requestBatch))
				v.consenter.ProcessBatch(requestBatch, v.consensusFailed)
			}
		}
	}
}

func (v *Verification) ProcessTransaction(tx *types.Transaction) bool {
	if !v.isLegalTransaction(tx) {
		return false
	}

	address, err := tx.Verfiy()
	if err != nil || !bytes.Equal(address.Bytes(), tx.Sender().Bytes()) {
		log.Debugf("[validator] illegal transaction %s: invalid signature", tx.Hash())
		return false
	}

	v.rwInTxs.Lock()
	if v.isExist(tx) {
		v.rwInTxs.Unlock()
		return false
	}

	if v.isOverCapacity() {
		elem := v.txpool.RemoveFront()
		delete(v.inTxs, elem.(*types.Transaction).Hash())
		log.Warnf("[validator]  excess capacity, remove front transaction")
	}

	v.txpool.Add(tx)
	v.inTxs[tx.Hash()] = tx
	cnt := v.txpool.Len()
	v.rwInTxs.Unlock()
	if cnt == 1 {
		v.requestBatchTimer.Reset(v.consenter.BatchTimeout())
	}
	v.requestBatchSignal <- cnt
	log.Debugf("[txPool] add transaction success, tx_hash: %s,txpool_len: %d", tx.Hash().String(), cnt)
	return true
}

func (v *Verification) consensusFailed(flag int, txs types.Transactions) {
	switch flag {
	// not use verify
	case 0:
		log.Debug("[validator] not use ...")

	// use verify
	case 1:
		log.Debug("[validator] use ...")
		var elems []sortedlinkedlist.IElement
		for _, tx := range txs {
			elems = append(elems, tx)
		}
		v.txpool.Removes(elems)
	// consensus failed
	case 2:
		log.Debug("[validator] consensus failed & verified...")
		v.rwInTxs.Lock()
		defer v.rwInTxs.Unlock()
		for _, tx := range txs {
			v.RollBackAccount(tx)
			v.txpool.Add(tx)
		}
	// consensus succeed
	case 3:
		log.Debug("[validator] consensus succeed...")
	default:
		log.Error("[validator] not support this flag ...")
	}
}

func (v *Verification) VerifyTxs(txs types.Transactions, primary bool) (bool, types.Transactions) {
	if !v.config.IsValid {
		return true, txs
	}
	v.rwInTxs.Lock()
	v.rwAccount.Lock()
	defer v.rwInTxs.Unlock()
	defer v.rwAccount.Unlock()
	var ttxs types.Transactions
	for _, tx := range txs {
		if !v.isExist(tx) {
			if !v.isLegalTransaction(tx) {
				if primary {
					log.Warnf("[validator] illegal ,tx_hash: %s", tx.Hash().String())
					v.txpool.Remove(tx)
					delete(v.inTxs, tx.Hash())
					continue
				} else {
					log.Errorf("[validator] illegal ,tx_hash: %s", tx.Hash().String())
					for _, rollbackTx := range ttxs {
						v.rollBackAccount(rollbackTx)
					}
					return false, nil
				}
			}
		}

		assetID := tx.AssetID()
		if tx.GetType() != types.TypeIssue {
			asset, ok := v.assets[assetID]
			if !ok {
				asset, _ = v.ledger.GetAssetFromDB(assetID)
			}
			if asset == nil {
				if primary {
					log.Warnf("[validator] asset id %d not exist, tx_hash: %s", tx.AssetID(), tx.Hash().String())
					continue
				} else {
					log.Errorf("[validator] asset id %d not exist, tx_hash: %s", tx.AssetID(), tx.Hash().String())
					for _, rollbackTx := range ttxs {
						v.rollBackAccount(rollbackTx)
					}
					return false, nil
				}
			} else if tx.GetType() == types.TypeIssueUpdate {
				if _, err := asset.Update(string(tx.Payload)); err != nil {
					if primary {
						log.Warnf("[validator] issue update asset %d(%s) : err %s, tx_hash: %s", assetID, string(tx.Payload), err, tx.Hash().String())
						continue
					} else {
						log.Errorf("[validator] issue update asset %d(%s) :err %s, tx_hash: %s", assetID, string(tx.Payload), err, tx.Hash().String())
						for _, rollbackTx := range ttxs {
							v.rollBackAccount(rollbackTx)
						}
						return false, nil
					}
				}
			}
		} else {
			if _, ok := v.assets[assetID]; !ok {
				asset := &state.Asset{
					ID: assetID,
				}
				newAsset, err := asset.Update(string(tx.Payload))
				if err != nil {
					if primary {
						log.Warnf("[validator] issue asset %d(%s) : err %s, tx_hash: %s", assetID, string(tx.Payload), err, tx.Hash().String())
						continue
					} else {
						log.Errorf("[validator] issue asset %d(%s) :err %s, tx_hash: %s", assetID, string(tx.Payload), err, tx.Hash().String())
						for _, rollbackTx := range ttxs {
							v.rollBackAccount(rollbackTx)
						}
						return false, nil
					}
				}
				v.assets[assetID] = newAsset
			} else {
				if primary {
					log.Warnf("[validator] issue asset %d(%s) : already exist, tx_hash: %s", assetID, string(tx.Payload), tx.Hash().String())
					continue
				} else {
					log.Errorf("[validator] issue asset %d(%s) : already exist, tx_hash: %s", assetID, string(tx.Payload), tx.Hash().String())
					for _, rollbackTx := range ttxs {
						v.rollBackAccount(rollbackTx)
					}
					return false, nil
				}
			}
		}

		// remove balance is negative tx
		if !v.updateAccount(tx) {
			if primary {
				log.Warnf("[validator] balance is negative ,tx_hash: %s", tx.Hash().String())
				continue
			} else {
				log.Errorf("[validator] balance is negative ,tx_hash: %s", tx.Hash().String())
				for _, rollbackTx := range ttxs {
					v.rollBackAccount(rollbackTx)
				}
				return false, nil
			}
		}
		ttxs = append(ttxs, tx)
	}

	return true, ttxs
}

func (v *Verification) RemoveTxsInVerification(txs types.Transactions) {
	v.rwInTxs.Lock()
	defer v.rwInTxs.Unlock()
	for _, tx := range txs {
		log.Debugf("[validator] remove transaction in verification ,tx_hash: %s ,txpool_len: %d", tx.Hash(), v.txpool.Len())
		delete(v.inTxs, tx.Hash())
		v.txpool.Remove(tx)
	}
}

func (v *Verification) fetchAccount(address accounts.Address) *state.Balance {
	account, ok := v.accounts[address.String()]
	if !ok {
		account, _ = v.ledger.GetBalanceFromDB(address)
		v.accounts[address.String()] = account
	}
	return account
}

func (v *Verification) updateAccount(tx *types.Transaction) bool {
	assetID := tx.AssetID()
	plusAmount := big.NewInt(tx.Amount().Int64())
	plusFee := big.NewInt(tx.Fee().Int64())
	subAmount := big.NewInt(int64(0)).Neg(tx.Amount())
	subFee := big.NewInt(int64(0)).Neg(tx.Fee())

	senderAccont := v.fetchAccount(tx.Sender())
	if senderAccont != nil {
		senderAccont.Add(assetID, subAmount)
		senderAccont.Add(assetID, subFee)
		//	log.Debugln("[validator] updateAccount sender: ", tx.Sender(), "amount: ", senderAccont.amount)
		if tx.GetType() != types.TypeIssue && senderAccont.Get(assetID).Sign() == -1 {
			senderAccont.Add(assetID, plusAmount)
			senderAccont.Add(assetID, plusFee)
			return false
		}
	}
	receiverAccount := v.fetchAccount(tx.Recipient())
	if receiverAccount != nil {
		receiverAccount.Add(assetID, plusAmount)
		receiverAccount.Add(assetID, plusFee)
		//	log.Debugln("[validator] updateAccount Recipient: ", tx.Recipient(), "amount: ", receiverAccount.amount)
		if receiverAccount.Get(assetID).Sign() == -1 {
			receiverAccount.Add(assetID, subAmount)
			receiverAccount.Add(assetID, subFee)
			return false
		}
	}

	return true
}

func (v *Verification) rollBackAccount(tx *types.Transaction) {
	assetID := tx.AssetID()
	plusAmount := big.NewInt(tx.Amount().Int64())
	plusFee := big.NewInt(tx.Fee().Int64())
	subAmount := big.NewInt(int64(0)).Neg(tx.Amount())
	subFee := big.NewInt(int64(0)).Neg(tx.Fee())

	senderAccont := v.fetchAccount(tx.Sender())
	if senderAccont != nil {
		senderAccont.Add(assetID, plusAmount)
		senderAccont.Add(assetID, plusFee)
	}
	receiverAccount := v.fetchAccount(tx.Recipient())
	if receiverAccount != nil {
		senderAccont.Add(assetID, subAmount)
		senderAccont.Add(assetID, subFee)
	}
}

func (v *Verification) UpdateAccount(tx *types.Transaction) bool {
	v.rwAccount.Lock()
	defer v.rwAccount.Unlock()
	return v.updateAccount(tx)
}

//RollBackAccount roll back account balance
func (v *Verification) RollBackAccount(tx *types.Transaction) {
	v.rwAccount.Lock()
	defer v.rwAccount.Unlock()
	v.rollBackAccount(tx)
}

func (v *Verification) GetTransactionByHash(txHash crypto.Hash) (*types.Transaction, bool) {
	if elem := v.txpool.GetIElementByKey(txHash.String()); elem != nil {
		return elem.(*types.Transaction), true
	}
	return nil, false
}

func (v *Verification) GetBalance(addr accounts.Address) *state.Balance {
	v.rwAccount.Lock()
	defer v.rwAccount.Unlock()
	acconut := v.fetchAccount(addr)
	return acconut
}