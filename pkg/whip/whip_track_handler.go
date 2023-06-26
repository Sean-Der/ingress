package whip

import (
	"bytes"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/livekit/ingress/pkg/errors"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/server-sdk-go/pkg/jitter"
	"github.com/livekit/server-sdk-go/pkg/synchronizer"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

const (
	maxVideoLatency = 600 * time.Millisecond
	maxAudioLatency = time.Second
)

type MediaSink interface {
	PushSample(s *media.Sample, ts time.Duration) error
	Close() error
}

type whipTrackHandler struct {
	logger       logger.Logger
	remoteTrack  *webrtc.TrackRemote
	receiver     *webrtc.RTPReceiver
	depacketizer rtp.Depacketizer
	jb           *jitter.Buffer
	mediaSink    MediaSink
	sync         *synchronizer.TrackSynchronizer
	writePLI     func(ssrc webrtc.SSRC)
	onRTCP       func(packet rtcp.Packet)

	firstPacket sync.Once
	fuse        core.Fuse
}

func newWHIPTrackHandler(
	logger logger.Logger,
	track *webrtc.TrackRemote,
	receiver *webrtc.RTPReceiver,
	sync *synchronizer.TrackSynchronizer,
	mediaSink MediaSink,
	writePLI func(ssrc webrtc.SSRC),
	onRTCP func(packet rtcp.Packet),
) (*whipTrackHandler, error) {
	t := &whipTrackHandler{
		logger:      logger,
		remoteTrack: track,
		receiver:    receiver,
		sync:        sync,
		mediaSink:   mediaSink,
		writePLI:    writePLI,
		onRTCP:      onRTCP,
		fuse:        core.NewFuse(),
	}

	jb, err := t.createJitterBuffer()
	if err != nil {
		return nil, err
	}
	t.jb = jb

	depacketizer, err := t.createDepacketizer()
	if err != nil {
		return nil, err
	}
	t.depacketizer = depacketizer

	return t, nil
}

func (t *whipTrackHandler) Start(onDone func(err error)) (err error) {
	t.startRTPReceiver(onDone)
	if t.onRTCP != nil {
		t.startRTCPReceiver()
	}

	return nil
}

func (t *whipTrackHandler) Close() {
	t.fuse.Break()
}

func (t *whipTrackHandler) startRTPReceiver(onDone func(err error)) {
	go func() {
		var err error

		defer func() {
			t.mediaSink.Close()
			if onDone != nil {
				onDone(err)
			}
		}()

		t.logger.Infow("starting rtp receiver")

		if t.remoteTrack.Kind() == webrtc.RTPCodecTypeVideo && t.writePLI != nil {
			t.writePLI(t.remoteTrack.SSRC())
		}

		for {
			select {
			case <-t.fuse.Watch():
				t.logger.Debugw("stopping rtp receiver")
				err = nil
				return
			default:
				err = t.processRTPPacket()
				switch err {
				case nil, errors.ErrPrerollBufferReset:
					// continue
				case io.EOF:
					err = nil
					return
				default:
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					}

					t.logger.Warnw("error reading rtp packets", err)
					return
				}
			}
		}
	}()
}

// TODO drain on close?
func (t *whipTrackHandler) processRTPPacket() error {
	var pkt *rtp.Packet

	_ = t.remoteTrack.SetReadDeadline(time.Now().Add(time.Millisecond * 500))

	pkt, _, err := t.remoteTrack.ReadRTP()
	if err != nil {
		return err
	}

	t.firstPacket.Do(func() {
		t.logger.Debugw("first packet received")
		t.sync.Initialize(pkt)
	})

	t.jb.Push(pkt)

	for {
		pkts := t.jb.Pop(false)
		if len(pkts) == 0 {
			break
		}

		var ts time.Duration
		var buffer bytes.Buffer // TODO reuse the same buffer across calls, after resetting it if buffer allocation is a performane bottleneck
		for _, pkt := range pkts {
			ts, err = t.sync.GetPTS(pkt)
			switch err {
			case nil, synchronizer.ErrBackwardsPTS:
				err = nil
			default:
				return err
			}

			if len(pkt.Payload) <= 2 {
				// Padding
				continue
			}

			buf, err := t.depacketizer.Unmarshal(pkt.Payload)
			if err != nil {
				return err
			}

			_, err = buffer.Write(buf)
			if err != nil {
				return err
			}
		}

		// This returns the average duration, not the actual duration of the specific sample
		// SampleBuilder is using the duration of the previous sample, which is inaccurate as well
		sampleDuration := t.sync.GetFrameDuration()

		s := &media.Sample{
			Data:     buffer.Bytes(),
			Duration: sampleDuration,
		}

		err = t.mediaSink.PushSample(s, ts)
		if err != nil {
			return err
		}
	}

	return nil
}

func (t *whipTrackHandler) startRTCPReceiver() {
	go func() {
		t.logger.Infow("starting app source rtcp receiver")

		for {
			select {
			case <-t.fuse.Watch():
				t.logger.Debugw("stopping app source rtcp receiver")
				return
			default:
				_ = t.receiver.SetReadDeadline(time.Now().Add(time.Millisecond * 500))
				pkts, _, err := t.receiver.ReadRTCP()

				switch {
				case err == nil:
					// continue
				case err == io.EOF:
					err = nil
					return
				default:
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					}

					t.logger.Warnw("error reading rtcp", err)
					return
				}

				for _, pkt := range pkts {
					t.onRTCP(pkt)
				}
			}
		}
	}()
}

func (t *whipTrackHandler) createDepacketizer() (rtp.Depacketizer, error) {
	var depacketizer rtp.Depacketizer

	switch strings.ToLower(t.remoteTrack.Codec().MimeType) {
	case strings.ToLower(webrtc.MimeTypeVP8):
		depacketizer = &codecs.VP8Packet{}

	case strings.ToLower(webrtc.MimeTypeH264):
		depacketizer = &codecs.H264Packet{}

	case strings.ToLower(webrtc.MimeTypeOpus):
		depacketizer = &codecs.OpusPacket{}

	default:
		return nil, errors.ErrUnsupportedDecodeFormat
	}

	return depacketizer, nil
}

func (t *whipTrackHandler) createJitterBuffer() (*jitter.Buffer, error) {
	var maxLatency time.Duration
	options := []jitter.Option{jitter.WithLogger(t.logger)}

	depacketizer, err := t.createDepacketizer()
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(t.remoteTrack.Codec().MimeType) {
	case strings.ToLower(webrtc.MimeTypeVP8):
		maxLatency = maxVideoLatency
		options = append(options, jitter.WithPacketDroppedHandler(func() { t.writePLI(t.remoteTrack.SSRC()) }))

	case strings.ToLower(webrtc.MimeTypeH264):
		maxLatency = maxVideoLatency
		options = append(options, jitter.WithPacketDroppedHandler(func() { t.writePLI(t.remoteTrack.SSRC()) }))

	case strings.ToLower(webrtc.MimeTypeOpus):
		maxLatency = maxAudioLatency
		// No PLI for audio

	default:
		return nil, errors.ErrUnsupportedDecodeFormat
	}

	clockRate := t.remoteTrack.Codec().ClockRate

	jb := jitter.NewBuffer(depacketizer, clockRate, maxLatency, options...)

	return jb, nil
}