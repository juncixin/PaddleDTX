// Copyright (c) 2021 PaddlePaddle Authors. All Rights Reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logic_reg_vl

import (
	"sync"

	"github.com/PaddlePaddle/PaddleDTX/crypto/common/math/homomorphism/paillier"
	"github.com/PaddlePaddle/PaddleDTX/xdb/errorx"
	"github.com/golang/protobuf/proto"
	"github.com/sirupsen/logrus"

	crypCom "github.com/PaddlePaddle/PaddleDTX/dai/crypto/vl/common"
	"github.com/PaddlePaddle/PaddleDTX/dai/errcodes"
	"github.com/PaddlePaddle/PaddleDTX/dai/mpc/psi"
	pbCom "github.com/PaddlePaddle/PaddleDTX/dai/protos/common"
	pb "github.com/PaddlePaddle/PaddleDTX/dai/protos/mpc"
	pbLogicRegVl "github.com/PaddlePaddle/PaddleDTX/dai/protos/mpc/learners/logic_reg_vl"
)

var (
	logger = logrus.WithField("module", "mpc.learners.logic_reg_vl")
)

// PSI is for vertical learning,
// initialized at the beginning of training by Learner
type PSI interface {
	// EncryptSampleIDSet to encrypt local IDs
	EncryptSampleIDSet() ([]byte, error)

	// SetReEncryptIDSet sets re-encrypted IDs from other party,
	// and tries to calculate final re-encrypted IDs
	// returns True if calculation is Done, otherwise False if still waiting for others' parts
	// returns Error if any mistake happens
	SetReEncryptIDSet(party string, reEncIDs []byte) (bool, error)

	// ReEncryptIDSet to encrypt encrypted IDs for other party
	ReEncryptIDSet(party string, encIDs []byte) ([]byte, error)

	// SetOtherFinalReEncryptIDSet sets final re-encrypted IDs of other party
	SetOtherFinalReEncryptIDSet(party string, reEncIDs []byte) error

	// IntersectParts tries to calculate intersection with all parties' samples
	// returns True with final result if calculation is Done, otherwise False if still waiting for others' samples
	// returns Error if any mistake happens
	// You'd better call it when SetReEncryptIDSet returns Done or SetOtherFinalReEncryptIDSet finishes
	IntersectParts() (bool, [][]string, []string, error)
}

// RpcHandler used to request remote mpc-node
type RpcHandler interface {
	StepTrain(req *pb.TrainRequest, peerName string) (*pb.TrainResponse, error)
}

// ResultHandler handles final result which is successful or failed
// Should be called when learning finished
type ResultHandler interface {
	SaveResult(*pbCom.TrainTaskResult)
}

type learnerStatusType uint8

const (
	learnerStatusStartPSI learnerStatusType = iota
	learnerStatusEndPSI
	learnerStatusStartTrain
	learnerStatusEndTrain
)

type Learner struct {
	id          string
	algo        pbCom.Algorithm
	address     string               // address indicates local mpc-node
	parties     []string             // parties are other learners who participates in MPC, assigned with mpc-node address usually
	homoPriv    *paillier.PrivateKey // homomorphic private key
	homoPub     []byte               // homomorphic public key for transfer
	trainParams *pbCom.TrainParams
	samplesFile []byte // sample file content for training model
	psi         PSI
	procMutex   sync.Mutex
	process     *process // process of training model
	loopRound   uint64
	rpc         RpcHandler    // rpc is used to request remote mpc-node
	rh          ResultHandler // rh handles final result which is successful or failed
	fileRows    [][]string    // fileRows returned by psi.IntersectParts

	status learnerStatusType
}

func (l *Learner) Advance(payload []byte) (*pb.TrainResponse, error) {
	m := &pbLogicRegVl.Message{}
	err := proto.Unmarshal(payload, m)
	if err != nil {
		return nil, errorx.New(errcodes.ErrCodeParam, "failed to Unmarshal payload: %s", err.Error())
	}

	return l.advance(m)
}

// advance handles all kinds of message
func (l *Learner) advance(message *pbLogicRegVl.Message) (*pb.TrainResponse, error) {
	mType := message.Type

	handleError := func(err error) {
		logger.WithField("error", err.Error()).Warning("failed to train out a model")
		res := &pbCom.TrainTaskResult{TaskID: l.id, ErrMsg: err.Error()}
		l.rh.SaveResult(res)
	}

	var ret *pb.TrainResponse
	switch mType {
	case pbLogicRegVl.MessageType_MsgPsiEnc: // local message
		encIDs, err := l.psi.EncryptSampleIDSet()
		if err != nil {
			go handleError(err)
			return nil, err
		}

		go func() {
			m := &pbLogicRegVl.Message{
				Type: pbLogicRegVl.MessageType_MsgPsiAskReEnc,
				VlLPsiReEncIDsReq: &pb.VLPsiReEncIDsRequest{
					TaskID: l.id,
					EncIDs: encIDs,
				},
			}
			l.advance(m)
		}()

	case pbLogicRegVl.MessageType_MsgPsiAskReEnc: // local message
		newMess := &pbLogicRegVl.Message{
			Type:              pbLogicRegVl.MessageType_MsgPsiReEnc,
			VlLPsiReEncIDsReq: message.VlLPsiReEncIDsReq,
			LoopRound:         l.loopRound,
		}
		reM, err := l.sendMessageWithRetry(newMess, l.parties[0])
		if err != nil {
			go handleError(err)
			return nil, err
		}

		done, err := l.psi.SetReEncryptIDSet(l.parties[0], reM.VlLPsiReEncIDsResp.ReEncIDs)
		if err != nil {
			go handleError(err)
			return nil, err
		}

		if done {
			go func() {
				m := &pbLogicRegVl.Message{
					Type: pbLogicRegVl.MessageType_MsgPsiIntersect,
				}
				l.advance(m)
			}()
		}

	case pbLogicRegVl.MessageType_MsgPsiReEnc:
		reEncIDs, err := l.psi.ReEncryptIDSet(message.From, message.VlLPsiReEncIDsReq.EncIDs)
		if err != nil {
			go handleError(err)
			return nil, err
		}

		retM := &pbLogicRegVl.Message{
			Type: pbLogicRegVl.MessageType_MsgPsiReEnc,
			To:   message.From,
			From: l.address,
			VlLPsiReEncIDsResp: &pb.VLPsiReEncIDsResponse{
				TaskID:   l.id,
				ReEncIDs: reEncIDs,
			},
		}
		payload, err := proto.Marshal(retM)
		if err != nil {
			err = errorx.New(errcodes.ErrCodeInternal, "failed to Marshal payload: %s", err.Error())
			go handleError(err)
			return nil, err
		}

		ret = &pb.TrainResponse{
			TaskID:  l.id,
			Payload: payload,
		}

		err = l.psi.SetOtherFinalReEncryptIDSet(message.From, reEncIDs)
		if err != nil {
			go handleError(err)
		} else {
			go func() {
				m := &pbLogicRegVl.Message{
					Type: pbLogicRegVl.MessageType_MsgPsiIntersect,
				}
				l.advance(m)
			}()
		}

	case pbLogicRegVl.MessageType_MsgPsiIntersect: // local message
		done, newRows, _, err := l.psi.IntersectParts()
		if err != nil {
			go handleError(err)
			return nil, err
		}

		if done {
			l.fileRows = newRows
			l.status = learnerStatusEndPSI
			go func() {
				m := &pbLogicRegVl.Message{
					Type: pbLogicRegVl.MessageType_MsgTrainHup,
				}
				l.advance(m)
			}()
		}

	case pbLogicRegVl.MessageType_MsgTrainHup: // local message
		l.procMutex.Lock()
		defer l.procMutex.Unlock()
		if learnerStatusEndPSI == l.status {
			l.status = learnerStatusStartTrain
			err := l.process.init(l.fileRows)
			if err != nil {
				go handleError(err)
				return nil, err
			}

			m := &pbLogicRegVl.Message{
				Type:       pbLogicRegVl.MessageType_MsgHomoPubkey,
				HomoPubkey: l.homoPub,
				LoopRound:  l.loopRound,
			}
			_, err = l.sendMessageWithRetry(m, l.parties[0])
			if err != nil {
				go handleError(err)
				return nil, err
			}

			go func() {
				m := &pbLogicRegVl.Message{
					Type:      pbLogicRegVl.MessageType_MsgTrainLoop,
					LoopRound: 0, // start Round-0
				}
				l.advance(m)
			}()
		}

	case pbLogicRegVl.MessageType_MsgHomoPubkey:
		homoPubkeyOfOther := message.HomoPubkey
		l.process.setHomoPubOfOther(homoPubkeyOfOther)
		ret = &pb.TrainResponse{
			TaskID: l.id,
		}

	case pbLogicRegVl.MessageType_MsgTrainLoop: // local message
		newRound := message.LoopRound
		l.procMutex.Lock()
		defer l.procMutex.Unlock()
		if newRound == 0 || newRound == l.loopRound+1 {
			l.loopRound = newRound
			err := l.process.upRound(l.loopRound)
			if err != nil {
				go handleError(err)
				return nil, err
			}
			go func() {
				m := &pbLogicRegVl.Message{
					Type:      pbLogicRegVl.MessageType_MsgTrainCalLocalGradCost,
					LoopRound: l.loopRound,
				}
				l.advance(m)
			}()
		}

	case pbLogicRegVl.MessageType_MsgTrainCalLocalGradCost: // local message
		loopRound := message.LoopRound
		if loopRound == l.loopRound {
			partBytesForOther, t, err := l.process.calLocalGradientAndCost()
			if err != nil {
				go handleError(err)
				return nil, err
			}

			if t == 1 {
				m := &pbLogicRegVl.Message{
					Type:      pbLogicRegVl.MessageType_MsgTrainPartBytes,
					PartBytes: partBytesForOther,
					LoopRound: loopRound,
				}
				_, err = l.sendMessageWithRetry(m, l.parties[0])
				if err != nil {
					go handleError(err)
					return nil, err
				}

				go func() {
					m := &pbLogicRegVl.Message{
						Type:      pbLogicRegVl.MessageType_MsgTrainCalEncGradCost,
						LoopRound: loopRound,
					}
					l.advance(m)
				}()
			}
		}

	case pbLogicRegVl.MessageType_MsgTrainPartBytes:
		loopRound := message.LoopRound
		partBytesFromOther := message.PartBytes
		if loopRound == l.loopRound || loopRound == l.loopRound+1 {
			err := l.process.setPartBytesFromOther(partBytesFromOther, loopRound)
			if err != nil {
				go handleError(err)
				return nil, err
			}
		}
		if loopRound == l.loopRound {
			go func() {
				m := &pbLogicRegVl.Message{
					Type:      pbLogicRegVl.MessageType_MsgTrainCalEncGradCost,
					LoopRound: loopRound,
				}
				l.advance(m)
			}()
		}

		ret = &pb.TrainResponse{
			TaskID: l.id,
		}

	case pbLogicRegVl.MessageType_MsgTrainCalEncGradCost: // local message
		loopRound := message.LoopRound
		if loopRound == l.loopRound {
			encGradForOther, encCostForOther, t, err := l.process.calEncGradientAndCost()
			if err != nil {
				go handleError(err)
				return nil, err
			}

			if t == 1 {
				m := &pbLogicRegVl.Message{
					Type:             pbLogicRegVl.MessageType_MsgTrainEncGradCost,
					EncGradFromOther: encGradForOther,
					EncCostFromOther: encCostForOther,
					LoopRound:        loopRound,
				}
				_, err = l.sendMessageWithRetry(m, l.parties[0])
				if err != nil {
					go handleError(err)
					return nil, err
				}
			} // else wait for message
		}

	case pbLogicRegVl.MessageType_MsgTrainEncGradCost:
		loopRound := message.LoopRound
		encGradFromOther := message.EncGradFromOther
		encCostFromOther := message.EncCostFromOther
		if loopRound == l.loopRound {
			t := l.process.setEncGradientAndCostFromOther(encGradFromOther, encCostFromOther)
			if t == 1 {
				go func() {
					m := &pbLogicRegVl.Message{
						Type:      pbLogicRegVl.MessageType_MsgTrainDecLocalGradCost,
						LoopRound: loopRound,
					}
					l.advance(m)
				}()
			}
		}
		ret = &pb.TrainResponse{
			TaskID: l.id,
		}

	case pbLogicRegVl.MessageType_MsgTrainDecLocalGradCost: // local message
		loopRound := message.LoopRound
		if loopRound == l.loopRound {
			gradBytesForOther, costBytesForOther, t, err := l.process.decGradientAndCost()
			if err != nil {
				go handleError(err)
				return nil, err
			}

			if t == 1 {
				m := &pbLogicRegVl.Message{
					Type:      pbLogicRegVl.MessageType_MsgTrainGradAndCost,
					GradBytes: gradBytesForOther,
					CostBytes: costBytesForOther,
					LoopRound: loopRound,
				}
				_, err = l.sendMessageWithRetry(m, l.parties[0])
				if err != nil {
					go handleError(err)
					return nil, err
				}
			}
		}

	case pbLogicRegVl.MessageType_MsgTrainGradAndCost:
		loopRound := message.LoopRound
		gradBytesFromOther := message.GradBytes
		costBytesFromOther := message.CostBytes
		if loopRound == l.loopRound {
			t := l.process.SetGradientAndCostFromOther(gradBytesFromOther, costBytesFromOther)
			if t == 1 {
				go func() {
					m := &pbLogicRegVl.Message{
						Type:      pbLogicRegVl.MessageType_MsgTrainUpdCostGrad,
						LoopRound: loopRound,
					}
					l.advance(m)
				}()
			}
		}
		ret = &pb.TrainResponse{
			TaskID: l.id,
		}

	case pbLogicRegVl.MessageType_MsgTrainUpdCostGrad: // local message
		loopRound := message.LoopRound
		if loopRound == l.loopRound {
			stopped, err := l.process.updateCostAndGradient()
			if err != nil {
				go handleError(err)
				return nil, err
			}

			m := &pbLogicRegVl.Message{
				Type:      pbLogicRegVl.MessageType_MsgTrainStatus,
				Stopped:   stopped,
				LoopRound: loopRound,
			}
			logger.Infof("learner[%s] send to remote learner[%s]'s status[%t], loopRound[%d].", l.id, l.parties[0], stopped, l.loopRound)
			_, err = l.sendMessageWithRetry(m, l.parties[0])
			if err != nil {
				go handleError(err)
				return nil, err
			}

			go func() {
				m := &pbLogicRegVl.Message{
					Type:      pbLogicRegVl.MessageType_MsgTrainCheckStatus,
					LoopRound: loopRound,
				}
				l.advance(m)
			}()
		}

	case pbLogicRegVl.MessageType_MsgTrainStatus:
		loopRound := message.LoopRound

		if loopRound == l.loopRound {
			otherStopped := message.Stopped
			logger.Infof("learner[%s] got remote learner[%s]'s status[%t], loopRound[%d].", l.id, message.From, otherStopped, l.loopRound)
			l.process.setOtherStatus(otherStopped)

			go func() {
				m := &pbLogicRegVl.Message{
					Type:      pbLogicRegVl.MessageType_MsgTrainCheckStatus,
					LoopRound: loopRound,
				}
				l.advance(m)
			}()
		}

		ret = &pb.TrainResponse{
			TaskID: l.id,
		}

	case pbLogicRegVl.MessageType_MsgTrainCheckStatus: // local message
		loopRound := message.LoopRound

		decided, stopped := l.process.stop()
		if decided {
			if stopped {
				logger.WithField("loopRound", l.loopRound).Infof("learner[%s] trained out a model this round[%d], got ready to stop.", l.id, loopRound)
				go func() {
					m := &pbLogicRegVl.Message{
						Type:      pbLogicRegVl.MessageType_MsgTrainModels,
						LoopRound: loopRound,
					}
					l.advance(m)
				}()
			} else {
				logger.WithField("loopRound", l.loopRound).Infof("learner[%s] did not train out model this round[%d], got ready to start new round[%d].", l.id, loopRound, loopRound+1)
				go func() {
					m := &pbLogicRegVl.Message{
						Type:      pbLogicRegVl.MessageType_MsgTrainLoop,
						LoopRound: loopRound + 1, //for starting new loop
					}
					l.advance(m)
				}()
			}
		}

	case pbLogicRegVl.MessageType_MsgTrainModels: // local message
		l.procMutex.Lock()
		defer l.procMutex.Unlock()
		if learnerStatusStartTrain == l.status {
			l.status = learnerStatusEndTrain
			model, err := l.process.getTrainModels()
			if err != nil {
				go handleError(err)
				return nil, err
			}
			logger.WithField("loopRound", l.loopRound).Infof("learner[%s] trained out model[%v] successfully.", l.id, model)
			res := &pbCom.TrainTaskResult{
				TaskID:  l.id,
				Success: true,
				Model:   model,
			}
			l.rh.SaveResult(res)
		}
	}

	logger.WithFields(logrus.Fields{
		"loopRound":    l.loopRound,
		"messageRound": message.LoopRound,
	}).Infof("learner[%s] finished advance . message %s", l.id, message.Type.String())
	return ret, nil
}

// sendMessageWithRetry sends message to remote mpc-node
// retries 2 times at most
func (l *Learner) sendMessageWithRetry(message *pbLogicRegVl.Message, address string) (*pbLogicRegVl.Message, error) {
	times := 3

	var m *pbLogicRegVl.Message
	var err error
	for i := 0; i < times; i++ {
		m, err = l.sendMessage(message, address)
		if err == nil {
			break
		}
	}

	return m, err
}

// sendMessage sends message to remote mpc-node
func (l *Learner) sendMessage(message *pbLogicRegVl.Message, address string) (*pbLogicRegVl.Message, error) {
	message.From = l.address
	message.To = address

	payload, err := proto.Marshal(message)
	if err != nil {
		return nil, errorx.New(errcodes.ErrCodeInternal, "failed to Marshal payload: %s", err.Error())
	}

	trainReq := &pb.TrainRequest{
		TaskID:  l.id,
		Algo:    l.algo,
		Payload: payload,
	}
	resp, err := l.rpc.StepTrain(trainReq, address)
	if err != nil {
		return nil, err
	}

	m := &pbLogicRegVl.Message{}
	if len(resp.Payload) != 0 {
		err := proto.Unmarshal(resp.Payload, m)
		if err != nil {
			return nil, errorx.New(errcodes.ErrCodeInternal, "failed to Unmarshal payload[%s] from[%s] and err is[%s] ", string(resp.Payload), address, err.Error())
		}
	}
	return m, nil
}

// NewLearner returns a VerticalLogicRegression Learner
// id is the assigned id for Learner
// address indicates local mpc-node
// parties are other learners who participates in MPC, assigned with mpc-node address usually
// rpc is used to request remote mpc-node
// rh handles final result which is successful or failed
// params are parameters for training model
// samplesFile contains samples for training model
func NewLearner(id string, address string, params *pbCom.TrainParams, samplesFile []byte,
	parties []string, rpc RpcHandler, rh ResultHandler) (*Learner, error) {

	p, err := psi.NewVLTowPartsPSI(address, samplesFile, params.GetIdName(), parties)
	if err != nil {
		return nil, err
	}

	homoPriv, homoPub, err := crypCom.GenerateHomoKeyPair()
	if err != nil {
		return nil, err
	}

	l := &Learner{
		id:          id,
		algo:        pbCom.Algorithm_LOGIC_REGRESSION_VL,
		address:     address,
		parties:     parties,
		homoPriv:    homoPriv,
		homoPub:     homoPub,
		psi:         p,
		trainParams: params,
		process:     newProcess(homoPriv, params),
		samplesFile: samplesFile,
		rpc:         rpc,
		rh:          rh,
		status:      learnerStatusStartPSI,
	}

	// start training
	go func() {
		m := &pbLogicRegVl.Message{
			Type: pbLogicRegVl.MessageType_MsgPsiEnc,
		}
		l.advance(m)
	}()
	return l, nil
}
