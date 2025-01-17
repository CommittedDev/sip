// Copyright 2024 LiveKit, Inc.
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

package lktest

import (
	"context"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/audiotest"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/opus"
	"github.com/livekit/sip/pkg/media/rtp"
	webmm "github.com/livekit/sip/pkg/media/webm"
	"github.com/livekit/sip/pkg/mixer"
)

const (
	channels = 1
)

func New(wsURL, apiKey, apiSecret string) *LiveKit {
	lk := &LiveKit{
		ApiKey:    apiKey,
		ApiSecret: apiSecret,
		WsUrl:     wsURL,
	}
	lk.Rooms = lksdk.NewRoomServiceClient(lk.WsUrl, lk.ApiKey, lk.ApiSecret)
	lk.SIP = lksdk.NewSIPClient(lk.WsUrl, lk.ApiKey, lk.ApiSecret)
	return lk
}

type LiveKit struct {
	Rooms     *lksdk.RoomServiceClient
	SIP       *lksdk.SIPClient
	ApiKey    string
	ApiSecret string
	WsUrl     string
}

func (lk *LiveKit) ListRooms(t TB) []*livekit.Room {
	resp, err := lk.Rooms.ListRooms(context.Background(), &livekit.ListRoomsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Rooms
}

func (lk *LiveKit) RoomParticipants(t TB, room string) []*livekit.ParticipantInfo {
	resp, err := lk.Rooms.ListParticipants(context.Background(), &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Participants
}

func (lk *LiveKit) CreateSIPParticipant(t TB, trunk, room, identity, name, meta, number, dtmf string) {
	_, err := lk.SIP.CreateSIPParticipant(context.Background(), &livekit.CreateSIPParticipantRequest{
		SipTrunkId:          trunk,
		SipCallTo:           number,
		RoomName:            room,
		ParticipantIdentity: identity,
		ParticipantName:     name,
		ParticipantMetadata: meta,
		Dtmf:                dtmf,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (lk *LiveKit) Connect(t TB, room, identity string, cb *lksdk.RoomCallback) *lksdk.Room {
	r := lksdk.NewRoom(cb)
	err := r.Join(lk.WsUrl, lksdk.ConnectInfo{
		APIKey:              lk.ApiKey,
		APISecret:           lk.ApiSecret,
		RoomName:            room,
		ParticipantIdentity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Disconnect)
	return r
}

func (lk *LiveKit) ConnectParticipant(t TB, room, identity string, cb *lksdk.RoomCallback) *Participant {
	if cb == nil {
		cb = new(lksdk.RoomCallback)
	}
	p := &Participant{t: t}
	pr, pw := media.Pipe[media.PCM16Sample]()
	t.Cleanup(func() {
		pw.Close()
		pr.Close()
	})
	p.AudioIn = pr
	p.mix = mixer.NewMixer(pw, rtp.DefFrameDur, rtp.DefSampleRate)
	cb.ParticipantCallback.OnTrackPublished = func(pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		if pub.Kind() == lksdk.TrackKindAudio {
			if err := pub.SetSubscribed(true); err != nil {
				t.Error("cannot subscribe to the track", pub.SID(), err)
			}
		}
	}
	cb.ParticipantCallback.OnTrackSubscribed = func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		inp := p.mix.NewInput()
		defer p.mix.RemoveInput(inp)

		odec, err := opus.Decode(inp, rtp.DefSampleRate, channels)
		if err != nil {
			return
		}
		h := rtp.NewMediaStreamIn[opus.Sample](odec)
		_ = rtp.HandleLoop(track, h)
	}
	p.Room = lk.Connect(t, room, identity, cb)
	track, err := p.newAudioTrack()
	if err != nil {
		t.Fatal(err)
	}
	p.AudioOut = track
	return p
}

type Participant struct {
	t   TB
	mix *mixer.Mixer

	Room     *lksdk.Room
	AudioOut media.Writer[media.PCM16Sample]
	AudioIn  media.Reader[media.PCM16Sample]
}

func (p *Participant) newAudioTrack() (media.Writer[media.PCM16Sample], error) {
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		return nil, err
	}
	pt := p.Room.LocalParticipant
	if _, err = pt.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: pt.Identity(),
	}); err != nil {
		return nil, err
	}
	ow := media.FromSampleWriter[opus.Sample](track, rtp.DefFrameDur)
	pw, err := opus.Encode(ow, rtp.DefSampleRate, channels)
	if err != nil {
		return nil, err
	}
	return pw, nil
}

const (
	signalAmp    = math.MaxInt16 / 4
	signalAmpMin = signalAmp - signalAmp/4 // TODO: why it's so low?
	signalAmpMax = signalAmp + signalAmp/10
)

func (p *Participant) SendSignal(ctx context.Context, n int, val int) error {
	signal := make(media.PCM16Sample, rtp.DefPacketDur)
	audiotest.GenSignal(signal, []audiotest.Wave{{Ind: val, Amp: signalAmp}})
	sid, id := p.Room.LocalParticipant.SID(), p.Room.LocalParticipant.Identity()
	p.t.Log("sending signal", "sid", sid, "id", id, "len", len(signal), "n", n, "sig", val)

	ticker := time.NewTicker(rtp.DefFrameDur)
	defer ticker.Stop()
	i := 0
	for {
		if n > 0 && i >= n {
			break
		}
		select {
		case <-ctx.Done():
			if n <= 0 {
				p.t.Log("stopping signal", "sid", sid, "id", id, "n", i, "sig", val)
				return nil
			}
			return ctx.Err()
		case <-ticker.C:
		}

		if err := p.AudioOut.WriteSample(signal); err != nil {
			return err
		}
		i++
	}
	return nil
}

func (p *Participant) WaitSignals(ctx context.Context, vals []int, w io.WriteCloser) error {
	var ws media.PCM16WriteCloser
	if w != nil {
		ws = webmm.NewPCM16Writer(w, rtp.DefSampleRate, rtp.DefFrameDur)
		defer ws.Close()
	}
	lastLog := time.Now()
	buf := make(media.PCM16Sample, rtp.DefPacketDur)
	sid, id := p.Room.LocalParticipant.SID(), p.Room.LocalParticipant.Identity()
	for {
		n, err := p.AudioIn.ReadSample(buf)
		if err != nil {
			p.t.Log("cannot read rtp packet", "err", err)
			return err
		}
		decoded := buf[:n]
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if ws != nil {
			if err = ws.WriteSample(decoded); err != nil {
				return err
			}
		}
		if !slices.ContainsFunc(decoded, func(v int16) bool { return v != 0 }) {
			continue // Ignore silence.
		}
		out := audiotest.FindSignal(decoded)
		if len(out) >= len(vals) {
			// Only consider first N strongest signals.
			out = out[:len(vals)]
			// Sort them again by index, so it's easier to compare.
			slices.SortFunc(out, func(a, b audiotest.Wave) int {
				return a.Ind - b.Ind
			})
			ok := true
			for i := range vals {
				// All signals must match the frequency and have around the same amplitude.
				if out[i].Ind != vals[i] || out[i].Amp < signalAmpMin || out[i].Amp > signalAmpMax {
					ok = false
					break
				}
			}
			if ok {
				p.t.Log("signal found", "sid", sid, "id", id, "sig", vals)
				return nil
			}
		}
		// Remove most other components from the logs.
		if len(out) > len(vals)*2 {
			out = out[:len(vals)*2]
		}
		if time.Since(lastLog) > time.Second {
			lastLog = time.Now()
			p.t.Log("skipping signal", "sid", sid, "id", id, "len", len(decoded), "signals", out)
		}
	}
}

type ParticipantInfo struct {
	Identity string
	Name     string
	Kind     livekit.ParticipantInfo_Kind
	Metadata string
}

func (lk *LiveKit) ExpectParticipants(t TB, ctx context.Context, room string, participants []ParticipantInfo) {
	var list []*livekit.ParticipantInfo
	ticker := time.NewTicker(time.Second / 4)
	defer ticker.Stop()
wait:
	for {
		list = lk.RoomParticipants(t, room)
		if len(list) == len(participants) {
			break
		}
		select {
		case <-ctx.Done():
			break wait
		case <-ticker.C:
		}
	}
	require.Len(t, list, len(participants))
	slices.SortFunc(participants, func(a, b ParticipantInfo) int {
		return strings.Compare(a.Identity, b.Identity)
	})
	slices.SortFunc(list, func(a, b *livekit.ParticipantInfo) int {
		return strings.Compare(a.Identity, b.Identity)
	})
	for i := range participants {
		exp, got := participants[i], list[i]
		require.Equal(t, exp.Identity, got.Identity)
		require.Equal(t, exp.Kind, got.Kind)
		if exp.Name != "" {
			require.Equal(t, exp.Name, got.Name)
		}
		require.Equal(t, exp.Metadata, got.Metadata)
	}
}

func (lk *LiveKit) waitRooms(t TB, ctx context.Context, none bool, filter func(r *livekit.Room) bool) []*livekit.Room {
	var rooms []*livekit.Room
	ticker := time.NewTicker(time.Second / 4)
	defer ticker.Stop()
	for {
		rooms = lk.ListRooms(t)
		if filter != nil {
			var out []*livekit.Room
			for _, r := range rooms {
				if filter(r) {
					out = append(out, r)
				}
			}
			rooms = out
		}
		if !none {
			if len(rooms) >= 1 {
				return rooms
			}
		} else {
			if len(rooms) == 0 {
				return rooms
			}
		}
		select {
		case <-ctx.Done():
			return rooms
		case <-ticker.C:
		}
	}
}

func (lk *LiveKit) ExpectRoomWithParticipants(t TB, ctx context.Context, room string, participants []ParticipantInfo) {
	filter := func(r *livekit.Room) bool {
		return r.Name == room
	}
	rooms := lk.waitRooms(t, ctx, len(participants) == 0, filter)
	if len(participants) == 0 && len(rooms) == 0 {
		return
	}
	require.Len(t, rooms, 1)
	require.True(t, filter(rooms[0]))

	lk.ExpectParticipants(t, ctx, room, participants)
}

func (lk *LiveKit) ExpectRoomPrefWithParticipants(t TB, ctx context.Context, pref, number string, participants []ParticipantInfo) {
	filter := func(r *livekit.Room) bool {
		return r.Name != pref && strings.HasPrefix(r.Name, pref+"_"+number+"_")
	}
	rooms := lk.waitRooms(t, ctx, len(participants) == 0, filter)
	require.Len(t, rooms, 1)
	require.True(t, filter(rooms[0]))
	t.Log("Room:", rooms[0].Name)

	lk.ExpectParticipants(t, ctx, rooms[0].Name, participants)
}
