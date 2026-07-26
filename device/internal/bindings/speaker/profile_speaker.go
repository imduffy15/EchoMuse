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

	// echoTap receives every period pumped to ALSA, silence included, so the
	// AEC far end stays aligned with real playback time rather than only with
	// the periods that carried audio. Set once before Init.
	echoTap func([]byte)
	// levelTap receives the RMS of each pumped period (0..1) and drives the
	// energy-reactive LED pattern. Must be fast; it runs on the pump loop.
	levelTap func(rms float64)

	statsMu sync.Mutex
	statsCb func(StreamStats)
	stats   StreamStats
	// primeWaitStart marks when the current stream began waiting to prime, so
	// PrimeWaitMs measures the real added start latency.
	primeWaitStart time.Time
	streamActive   bool
	// firstRecv/lastRecv bound the arrival span of the current stream, which
	// is what distinguishes "the wire could not keep up" from "playback
	// stuttered locally".
	firstRecv, lastRecv time.Time

	closeOnce sync.Once
	deadOnce  sync.Once
}

// OnStreamStats registers a per-stream stats callback, reported once when a
// stream reaches EOS. Invoked on its own goroutine so a slow consumer (the
// network send) can never stall the ALSA pump.
func (s *ProfileSpeaker) OnStreamStats(cb func(StreamStats)) {
	s.statsMu.Lock()
	s.statsCb = cb
	s.statsMu.Unlock()
}

var _ pkgspeaker.Speaker = (*ProfileSpeaker)(nil)

// audioChanDepth, primePeriods, StreamStats and periodRMS are shared with the
// tinyalsa backend and live in common.go.

// NewProfileSpeaker constructs a playback backend for the given profile.
//
// echoTap and levelTap have the same contract as NewPcmSpeaker's: the former
// feeds the AEC far end, the latter drives the LED level meter. Either may be
// nil.
//
// On a device with a hardware AEC reference (Profile.HasAECReference) the
// canceller can instead take the loopback channels straight off the capture
// stream, which is sample-aligned by construction. echoTap remains available
// so the software path still works and the two can be compared.
func NewProfileSpeaker(p *profile.Profile, echoTap func([]byte), levelTap func(rms float64)) *ProfileSpeaker {
	periodBytes := p.Speaker.PeriodSize * p.Speaker.FrameBytes()
	return &ProfileSpeaker{
		prof:            p,
		audioCh:         make(chan []byte, audioChanDepth),
		stopCh:          make(chan struct{}),
		deadCh:          make(chan struct{}),
		periodBytes:     periodBytes,
		monoPeriodBytes: periodBytes / p.Speaker.Channels,
		silence:         make([]byte, periodBytes),
		echoTap:         echoTap,
		levelTap:        levelTap,
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
		endOfStream := false

		if !primed && len(s.audioCh) < primePeriods && !s.eosPending.Load() {
			period = s.silence
		} else {
			if !primed {
				primed = true
				s.statsMu.Lock()
				if !s.primeWaitStart.IsZero() {
					s.stats.PrimeWaitMs = time.Since(s.primeWaitStart).Milliseconds()
				}
				s.statsMu.Unlock()
			}
			select {
			case p, ok := <-s.audioCh:
				if !ok {
					return
				}
				period = p
				s.statsMu.Lock()
				s.stats.Periods++
				if d := len(s.audioCh); !s.streamActive || d < s.stats.MinDepth {
					s.stats.MinDepth = d
				}
				s.streamActive = true
				s.statsMu.Unlock()
			default:
				// Nothing queued: either a clean end of stream or an underrun.
				if s.eosPending.Swap(false) {
					endOfStream = true
				} else {
					log.Printf("speaker: underrun — no audio queued")
					s.statsMu.Lock()
					s.stats.Underruns++
					s.stats.MinDepth = 0
					s.statsMu.Unlock()
				}
				primed = false
				period = s.silence
			}
		}

		if _, err := s.pcm.Write(period); err != nil {
			log.Printf("speaker: write error: %v", err)
			return
		}

		// Taps run for silence too, so the AEC far end stays aligned with real
		// playback time rather than only with the periods that carried audio.
		if s.echoTap != nil {
			s.echoTap(period)
		}
		if s.levelTap != nil {
			if endOfStream || &period[0] == &s.silence[0] {
				s.levelTap(0)
			} else {
				s.levelTap(periodRMS(period))
			}
		}

		if endOfStream {
			s.emitStreamStats()
		}
	}
}

// emitStreamStats reports and resets the current stream's counters. The
// callback runs on its own goroutine so a slow consumer (a network send)
// cannot stall the ALSA pump.
func (s *ProfileSpeaker) emitStreamStats() {
	s.statsMu.Lock()
	st := s.stats
	cb := s.statsCb
	if !s.firstRecv.IsZero() {
		st.RecvSpanMs = s.lastRecv.Sub(s.firstRecv).Milliseconds()
	}
	s.stats = StreamStats{}
	s.streamActive = false
	s.primeWaitStart = time.Time{}
	s.firstRecv, s.lastRecv = time.Time{}, time.Time{}
	s.statsMu.Unlock()

	if cb != nil {
		go cb(st)
	}
}

// PumpPeriod queues one MONO period; L and R are duplicated on the way in.
func (s *ProfileSpeaker) PumpPeriod(data []byte) error {
	select {
	case <-s.deadCh:
		return errDead
	default:
	}

	// Arrival telemetry: the gap between periods is the receive-side half of
	// the delivery picture, and MaxGapMs distinguishes a uniformly slow link
	// from one that stalled briefly.
	now := time.Now()
	s.statsMu.Lock()
	if s.firstRecv.IsZero() {
		s.firstRecv = now
		s.primeWaitStart = now
	} else if gap := now.Sub(s.lastRecv).Milliseconds(); gap > s.stats.MaxGapMs {
		s.stats.MaxGapMs = gap
	}
	s.lastRecv = now
	s.stats.BytesRecv += uint64(len(data))
	s.statsMu.Unlock()

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
