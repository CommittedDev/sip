// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/sip"
	"github.com/livekit/sip/version"
)

const shutdownTimer = time.Second * 5

type sipServiceStopFunc func()
type sipServiceActiveCallsFunc func() int

type Service struct {
	conf *config.Config
	log  logger.Logger

	psrpcServer rpc.SIPInternalServerImpl
	psrpcClient rpc.IOInfoClient
	bus         psrpc.MessageBus

	promServer   *http.Server
	rpcSIPServer rpc.SIPInternalServer

	sipServiceStop        sipServiceStopFunc
	sipServiceActiveCalls sipServiceActiveCallsFunc

	shutdown core.Fuse
	killed   atomic.Bool
}

func NewService(
	conf *config.Config, log logger.Logger, srv rpc.SIPInternalServerImpl, sipServiceStop sipServiceStopFunc,
	sipServiceActiveCalls sipServiceActiveCallsFunc, cli rpc.IOInfoClient, bus psrpc.MessageBus,
) *Service {
	s := &Service{
		conf: conf,
		log:  log,

		psrpcServer: srv,
		psrpcClient: cli,
		bus:         bus,

		sipServiceStop:        sipServiceStop,
		sipServiceActiveCalls: sipServiceActiveCalls,
	}
	if conf.PrometheusPort > 0 {
		s.promServer = &http.Server{
			Addr:    fmt.Sprintf(":%d", conf.PrometheusPort),
			Handler: promhttp.Handler(),
		}
	}
	return s
}

func (s *Service) Stop(kill bool) {
	s.shutdown.Break()
	s.killed.Store(kill)
}

func (s *Service) Run() error {
	s.log.Debugw("starting service", "version", version.Version)

	if s.promServer != nil {
		promListener, err := net.Listen("tcp", s.promServer.Addr)
		if err != nil {
			return err
		}
		defer promListener.Close()
		go func() {
			_ = s.promServer.Serve(promListener)
		}()
	}

	var err error
	if s.rpcSIPServer, err = rpc.NewSIPInternalServer(s.psrpcServer, s.bus); err != nil {
		return err
	}
	defer s.rpcSIPServer.Shutdown()

	if err := s.RegisterCreateSIPParticipantTopic(); err != nil {
		return err
	}

	s.log.Debugw("service ready")

	for { //nolint: gosimple
		select {
		case <-s.shutdown.Watch():
			s.log.Infow("shutting down")
			s.DeregisterCreateSIPParticipantTopic()

			if !s.killed.Load() {
				activeCalls := s.sipServiceActiveCalls()
				if activeCalls > 0 {
					fmt.Printf("instance waiting for %d calls to finish", activeCalls)
					time.Sleep(shutdownTimer)
					continue
				}
			}

			s.sipServiceStop()
			return nil
		}
	}
}

func (s *Service) GetAuthCredentials(ctx context.Context, from, to, toHost, srcAddress string) (username, password string, drop bool, err error) {
	resp, err := s.psrpcClient.GetSIPTrunkAuthentication(ctx, &rpc.GetSIPTrunkAuthenticationRequest{
		From:       from,
		To:         to,
		ToHost:     toHost,
		SrcAddress: srcAddress,
	})

	if err != nil {
		return "", "", false, err
	}

	return resp.Username, resp.Password, resp.Drop, nil
}

func (s *Service) DispatchCall(ctx context.Context, info *sip.CallInfo) sip.CallDispatch {
	resp, err := s.psrpcClient.EvaluateSIPDispatchRules(ctx, &rpc.EvaluateSIPDispatchRulesRequest{

		CallingNumber: info.FromUser,
		CalledNumber:  info.ToUser,
		CalledHost:    info.ToHost,
		SrcAddress:    info.SrcAddress,
		Pin:           info.Pin,
		NoPin:         info.NoPin,
	})

	if err != nil {
		s.log.Warnw("SIP handle dispatch rule error", err)
		return sip.CallDispatch{Result: sip.DispatchNoRuleReject}
	}
	switch resp.Result {
	default:
		s.log.Errorw("SIP handle dispatch rule error", fmt.Errorf("unexpected dispatch result: %v", resp.Result))
		return sip.CallDispatch{Result: sip.DispatchNoRuleReject}
	case rpc.SIPDispatchResult_LEGACY_ACCEPT_OR_PIN:
		if resp.RequestPin {
			return sip.CallDispatch{Result: sip.DispatchRequestPin}
		}
		// TODO: finally deprecate and drop
		return sip.CallDispatch{
			Result:         sip.DispatchAccept,
			RoomName:       resp.RoomName,
			Identity:       resp.ParticipantIdentity,
			Name:           resp.ParticipantName,
			Metadata:       resp.ParticipantMetadata,
			WsUrl:          resp.WsUrl,
			Token:          resp.Token,
			TrunkID:        resp.SipTrunkId,
			DispatchRuleID: resp.SipDispatchRuleId,
		}
	case rpc.SIPDispatchResult_ACCEPT:
		return sip.CallDispatch{
			Result:         sip.DispatchAccept,
			RoomName:       resp.RoomName,
			Identity:       resp.ParticipantIdentity,
			Name:           resp.ParticipantName,
			Metadata:       resp.ParticipantMetadata,
			WsUrl:          resp.WsUrl,
			Token:          resp.Token,
			TrunkID:        resp.SipTrunkId,
			DispatchRuleID: resp.SipDispatchRuleId,
		}
	case rpc.SIPDispatchResult_REQUEST_PIN:
		return sip.CallDispatch{
			Result:  sip.DispatchRequestPin,
			TrunkID: resp.SipTrunkId,
		}
	case rpc.SIPDispatchResult_REJECT:
		return sip.CallDispatch{Result: sip.DispatchNoRuleReject}
	case rpc.SIPDispatchResult_DROP:
		return sip.CallDispatch{Result: sip.DispatchNoRuleDrop}
	}
}

func (s *Service) CanAccept() bool {
	return !s.shutdown.IsBroken()
}

func (s *Service) RegisterCreateSIPParticipantTopic() error {
	if s.rpcSIPServer != nil {
		return s.rpcSIPServer.RegisterCreateSIPParticipantTopic(s.conf.ClusterID)
	}

	return nil
}

func (s *Service) DeregisterCreateSIPParticipantTopic() {
	if s.rpcSIPServer != nil {
		s.rpcSIPServer.DeregisterCreateSIPParticipantTopic(s.conf.ClusterID)
	}
}
