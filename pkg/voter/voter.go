/**
 * Copyright (C) 2021 The poly network Authors
 * This file is part of The poly network library.
 *
 * The poly network is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Lesser General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * The poly network is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Lesser General Public License for more details.
 *
 * You should have received a copy of the GNU Lesser General Public License
 * along with the poly network.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package voter

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ontio/ontology/smartcontract/service/native/cross_chain/cross_chain_manager"
	"github.com/polynetwork/arb-voter/config"
	"github.com/polynetwork/arb-voter/pkg/db"
	"github.com/polynetwork/arb-voter/pkg/log"
	"github.com/polynetwork/eth-contracts/go_abi/eccm_abi"
	sdk "github.com/polynetwork/poly-go-sdk"
	"github.com/polynetwork/poly/common"
	"github.com/polynetwork/poly/core/types"
	common2 "github.com/polynetwork/poly/native/service/cross_chain_manager/common"
	autils "github.com/polynetwork/poly/native/service/utils"
)

type Voter struct {
	polySdk      *sdk.PolySdk
	signer       *sdk.Account
	conf         *config.Config
	clients      []*ethclient.Client
	bdb          *db.BoltDB
	contracts    []*eccm_abi.EthCrossChainManager
	contractAddr ethcommon.Address
	idx          int
}

func New(polySdk *sdk.PolySdk, signer *sdk.Account, conf *config.Config) *Voter {
	return &Voter{polySdk: polySdk, signer: signer, conf: conf}
}

func (v *Voter) init() (err error) {

	if v.conf.ArbConfig.BlocksToWait > ARB_USEFUL_BLOCK_NUM {
		ARB_USEFUL_BLOCK_NUM = v.conf.ArbConfig.BlocksToWait
	}

	var clients []*ethclient.Client
	for _, node := range v.conf.ArbConfig.RestURL {
		client, err := ethclient.Dial(node)
		if err != nil {
			log.Fatalf("ethclient.Dial failed:%v", err)
		}

		clients = append(clients, client)
	}
	v.clients = clients

	bdb, err := db.NewBoltDB(v.conf.BoltDbPath)
	if err != nil {
		return
	}

	v.bdb = bdb

	v.contractAddr = ethcommon.HexToAddress(v.conf.ArbConfig.ECCMContractAddress)
	v.contracts = make([]*eccm_abi.EthCrossChainManager, len(clients))
	for i := 0; i < len(v.clients); i++ {
		contract, err := eccm_abi.NewEthCrossChainManager(v.contractAddr, v.clients[i])
		if err != nil {
			return err
		}
		v.contracts[i] = contract
	}

	return
}

var ARB_USEFUL_BLOCK_NUM = uint64(1)

func (v *Voter) Start(ctx context.Context) {
	err := v.init()
	if err != nil {
		log.Fatalf("Voter.init failed: %v", err)
	}

	nextArbHeight := v.bdb.GetArbHeight()
	if v.conf.ForceConfig.ArbHeight > 0 {
		nextArbHeight = v.conf.ForceConfig.ArbHeight
	}
	ticker := time.NewTicker(time.Second * 2)
	for {
		select {
		case <-ticker.C:
			v.idx = randIdx(len(v.clients))
			height, err := ethGetCurrentHeight(v.conf.ArbConfig.RestURL[v.idx])
			if err != nil {
				log.Warnf("ethGetCurrentHeight failed:%v", err)
				continue
			}
			if height < nextArbHeight+ARB_USEFUL_BLOCK_NUM {
				log.Infof("height(%d) < nextArbHeight(%d)+ARB_USEFUL_BLOCK_NUM(%d)", height, nextArbHeight, ARB_USEFUL_BLOCK_NUM)
				continue
			}

			for nextArbHeight < height-ARB_USEFUL_BLOCK_NUM {
				select {
				case <-ctx.Done():
					return
				default:
				}
				log.Infof("handling arb height:%d", nextArbHeight)
				err = v.fetchLockDepositEvents(nextArbHeight)
				if err != nil {
					log.Warnf("fetchLockDepositEvents failed:%v", err)
					sleep()
					continue
				}
				nextArbHeight++
			}

			log.Infof("nextArbHeight arb height:%d", nextArbHeight)
			err = v.bdb.UpdateArbHeight(nextArbHeight)
			if err != nil {
				log.Warnf("UpdateArbHeight failed:%v", err)
			}

		case <-ctx.Done():
			log.Info("quiting from signal...")
			return
		}
	}
}

type CrossTransfer struct {
	txIndex string
	txId    []byte
	value   []byte
	toChain uint32
	height  uint64
}

func (v *Voter) fetchLockDepositEvents(height uint64) (err error) {
	contract := v.contracts[v.idx]

	opt := &bind.FilterOpts{
		Start:   height,
		End:     &height,
		Context: context.Background(),
	}
	events, err := contract.FilterCrossChainEvent(opt, nil)
	if err != nil {
		return
	}

	empty := true
	for events.Next() {
		evt := events.Event
		if evt.Raw.Address != v.contractAddr {
			log.Warnf("event source contract invalid: %s, expect: %s, height: %d", evt.Raw.Address.Hex(), v.contractAddr.Hex(), height)
			continue
		}
		param := &common2.MakeTxParam{}
		_ = param.Deserialization(common.NewZeroCopySource([]byte(evt.Rawdata)))
		if !v.conf.IsWhitelistMethod(param.Method) {
			log.Warnf("target contract method invalid %s, height: %d", param.Method, height)
			continue
		}

		empty = false
		raw, _ := v.polySdk.GetStorage(autils.CrossChainManagerContractAddress.ToHexString(),
			append(append([]byte(cross_chain_manager.DONE_TX), autils.GetUint64Bytes(v.conf.ArbConfig.SideChainId)...), param.CrossChainID...))
		if len(raw) != 0 {
			log.Infof("fetchLockDepositEvents - ccid %s (tx_hash: %s) already on poly",
				hex.EncodeToString(param.CrossChainID), evt.Raw.TxHash.Hex())
			continue
		}

		index := big.NewInt(0)
		index.SetBytes(evt.TxId)
		crossTx := &CrossTransfer{
			txIndex: encodeBigInt(index),
			txId:    evt.Raw.TxHash.Bytes(),
			toChain: uint32(evt.ToChainId),
			value:   []byte(evt.Rawdata),
			height:  height,
		}

		var txHash string
		txHash, err = v.commitVote(uint32(height), crossTx.value, crossTx.txId)
		if err != nil {
			log.Errorf("commitVote failed:%v", err)
			return
		}
		err = v.waitTx(txHash)
		if err != nil {
			log.Errorf("waitTx failed:%v txHash:%s", err,txHash)
			return
		}
	}

	log.Infof("arb height %d empty: %v", height, empty)

	return
}

func (v *Voter) waitTx(txHash string) (err error) {
	start := time.Now()
	var tx *types.Transaction
	for {
		tx, err = v.polySdk.GetTransaction(txHash)
		if tx == nil || err != nil {
			if time.Since(start) > time.Minute*5 {
				err = fmt.Errorf("waitTx timeout")
				return
			}
			time.Sleep(time.Second)
			continue
		}
		return
	}
}

func (v *Voter) commitVote(height uint32, value []byte, txhash []byte) (string, error) {
	log.Infof("commitVote, height: %d, value: %s, txhash: %s", height, hex.EncodeToString(value), hex.EncodeToString(txhash))
	tx, err := v.polySdk.Native.Ccm.ImportOuterTransfer(
		v.conf.ArbConfig.SideChainId,
		value,
		height,
		nil,
		v.signer.Address[:],
		[]byte{},
		v.signer)
	if err != nil {
		return "", err
	} else {
		log.Infof("commitVote - send transaction to poly chain: ( poly_txhash: %s, eth_txhash: %s, height: %d )",
			tx.ToHexString(), ethcommon.BytesToHash(txhash).String(), height)
		return tx.ToHexString(), nil
	}
}

func sleep() {
	time.Sleep(time.Second)
}
func randIdx(size int) int {
	return int(rand.Uint32()) % size
}
