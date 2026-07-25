package speaker

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wilbowes/EchoMuse/internal/alsa"
	"github.com/wilbowes/EchoMuse/internal/profile"
	pkgspeaker "github.com/wilbowes/EchoMuse/pkg/speaker"
)

// ProfileSpeaker is a profile-driven playback backend on the dependency-free
// ALSA client.
//
// The wire carries MONO; PumpPeriod duplicates L=R before queueing. The stereo
// ALSA config is an I2S/codec-path constraint, not a wire requirement, and
// both supported devices drive a single speaker.
//
// The stream is held open and fed silence when idle rather than being stopped
// between utterances. On checkers that is load-bearing: the RT5616 mutes LOUT
// on DAPM power-down (rt5616_lout_event, PRE_PMD) and unmutes on power-up, so
// stopping the stream between clips would cycle the output stage and its
// depop logic on every reply.
type ProfileSpeaker struct {
	prof *profile.Profile
	pcm  *alsa.PCM

	audioCh chan []byte
	stopCh  chan struct{}
	// deadCh is closed by the writer loop on any exit so PumpPeriod returns an
	// error rather than blocking forever on a dead consumer.
	deadCh chan struct{}

	// eosPending is set by EndStream and consumed when audioCh drains, so a
	// drain at natural end of stream is not misreported as an underrun.
	eosPending atomic.Bool

	periodBytes     int
	monoPeriodBytes int
	silence         []byte

	closeOnce sync.Once
	deadOnce  sync.Once
}

var _ pkgspeaker.Speaker = (*ProfileSpeaker)(nil)

// audioChanDepth — the WS sender delivers well above realtime, so its lead
// over playback grows until it hits this cap. Deep enough that a WiFi stall
// shorter than the accumulated lead cannot drain the channel mid-stream.
const audioChanDepth = 128

// primePeriods — playback holds on silence until this many periods are queued
// (or EOS arrives, for clips shorter than the prime). Protects the opening of
// playback, when the sender's lead is still near zero.
const primePeriods = 24

// NewProfileSpeaker constructs a playback backend for the given profile.
func NewProfileSpeaker(p *profile.Profile) *ProfileSpeaker {
	periodBytes := p.Speaker.PeriodSize * p.Speaker.FrameBytes()
	return &ProfileSpeaker{
		prof:            p,
		audioCh:         make(chan []byte, audioChanDepth),
		stopCh:          make(chan struct{}),
		deadCh:          make(chan struct{}),
		periodBytes:     periodBytes,
		monoPeriodBytes: periodBytes / p.Speaker.Channels,
		silence:         make([]byte, periodBytes),
	}
}

// Init opens the playback device and starts the writer loop.
func (s *ProfileSpeaker) Init() error {
	cfg := alsa.Config{
		Card:       s.prof.Speaker.Card,
		Device:     s.prof.Speaker.Device,
		Playback:   true,
		Channels:   s.prof.Speaker.Channels,
		Format:     s.prof.Speaker.Format,
		Rate:       s.prof.Speaker.SampleRate,
		PeriodSize: s.prof.Speaker.PeriodSize,
		Periods:    s.prof.Speaker.Periods,
	}
	pcm, err := alsa.Open(cfg)
	if err != nil {
		return err
	}
	s.pcm = pcm
	log.Printf("speaker: %s card %d device %d — %d ch %v @ %d Hz, period %d x %d",
		s.prof.Name, cfg.Card, cfg.Device, cfg.Channels, cfg.Format, cfg.Rate,
		cfg.PeriodSize, cfg.Periods)
	go s.writeLoop()
	return nil
}

// writeLoop keeps the PCM fed at all times, with queued audio when available
// and silence otherwise.
func (s *ProfileSpeaker) writeLoop() {
	defer s.markDead()

	primed := false
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		var period []byte
		if !primed && len(s.audioCh) < primePeriods && !s.eosPending.Load() {
			period = s.silence
		} else {
			primed = true
			select {
			case p, ok := <-s.audioCh:
				if !ok {
					return
				}
				period = p
			default:
				// Nothing queued: either a clean end of stream or an underrun.
				if s.eosPending.Swap(false) {
					primed = false
				} else {
					log.Printf("speaker: underrun — no audio queued")
					primed = false
				}
				period = s.silence
			}
		}

		if _, err := s.pcm.Write(period); err != nil {
			log.Printf("speaker: write error: %v", err)
			return
		}
	}
}

// PumpPeriod queues one MONO period; L and R are duplicated on the way in.
func (s *ProfileSpeaker) PumpPeriod(data []byte) error {
	select {
	case <-s.deadCh:
		return errDead
	default:
	}

	stereo := make([]byte, 0, len(data)*s.prof.Speaker.Channels)
	sampleBytes := s.prof.Speaker.Format.Bytes()
	for i := 0; i+sampleBytes <= len(data); i += sampleBytes {
		for c := 0; c < s.prof.Speaker.Channels; c++ {
			stereo = append(stereo, data[i:i+sampleBytes]...)
		}
	}

	select {
	case s.audioCh <- stereo:
		return nil
	case <-s.deadCh:
		return errDead
	case <-time.After(5 * time.Second):
		return errQueueFull
	}
}

// EndStream marks the current stream complete so the writer can tell a clean
// drain from a mid-stream underrun.
func (s *ProfileSpeaker) EndStream() { s.eosPending.Store(true) }

// Flush discards queued-but-unplayed audio immediately, for barge-in.
func (s *ProfileSpeaker) Flush() {
	for {
		select {
		case <-s.audioCh:
		default:
			s.eosPending.Store(false)
			return
		}
	}
}

// Close stops playback, silences the amp and releases the device.
func (s *ProfileSpeaker) Close() {
	s.closeOnce.Do(func() {
		close(s.stopCh)
		if s.pcm != nil {
			_ = s.pcm.Close()
		}
		s.prof.SilenceAmp()
	})
}

func (s *ProfileSpeaker) markDead() { s.deadOnce.Do(func() { close(s.deadCh) }) }

type speakerError string

func (e speakerError) Error() string { return string(e) }

const (
	errDead      = speakerError("speaker: playback loop is not running")
	errQueueFull = speakerError("speaker: timed out queueing audio")
)
