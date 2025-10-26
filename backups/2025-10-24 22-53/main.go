package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/faiface/beep"
	"github.com/faiface/beep/mp3"
	"github.com/faiface/beep/speaker"
	"github.com/faiface/beep/wav"
	"gopkg.in/yaml.v3"
)

// ----- Data model -----

type Project struct {
	Audio    string             `yaml:"audio"`
	OffsetMS int                `yaml:"offset_ms"`
	BPM      float64            `yaml:"bpm"`
	Artnet   ArtnetConfig       `yaml:"artnet"`
	Profiles map[string]Profile `yaml:"profiles"`
	Patch    []PatchedFixture   `yaml:"patch"`
	Events   []Event            `yaml:"events"`
}

type ArtnetConfig struct {
	Target   string `yaml:"target"`
	Universe uint16 `yaml:"universe"`
}

// Fixture profiles & patch (multi-channel)
type Profile struct {
	Channels map[string]int `yaml:"channels"` // e.g. {"dim":1,"r":2,"g":3,"b":4}
}

type PatchedFixture struct {
	ID      string `yaml:"id"`      // logical fixture id used in events, e.g. "front_wash"
	Profile string `yaml:"profile"` // key into Profiles map, e.g. "par_rgb"
	Base    int    `yaml:"base"`    // 1-based DMX start address
}

type Event struct {
	At       string         `yaml:"at"`
	Set      *SetEvent      `yaml:"set,omitempty"`
	Fade     *FadeEvent     `yaml:"fade,omitempty"`
	TempSet  *TempSetEvent  `yaml:"temp_set,omitempty"`
	TempFade *TempFadeEvent `yaml:"temp_fade,omitempty"`
	// internal fields after compile:
	atMS int64
}

type SetEvent struct {
	Fixture string         `yaml:"fixture,omitempty"`
	Ch      int            `yaml:"ch,omitempty"`
	Value   int            `yaml:"value,omitempty"`
	Values  map[string]int `yaml:"values,omitempty"`
}

type FadeEvent struct {
	Fixture string         `yaml:"fixture,omitempty"`
	Ch      int            `yaml:"ch,omitempty"`
	From    int            `yaml:"from,omitempty"`
	To      int            `yaml:"to,omitempty"`
	FromMap map[string]int `yaml:"from_map,omitempty"`
	ToMap   map[string]int `yaml:"to_map,omitempty"`
	Dur     string         `yaml:"dur"` // e.g. "1s", "500ms"
	startMS int64
	endMS   int64
}

type TempSetEvent struct {
	Fixture string         `yaml:"fixture,omitempty"`
	ToMap   map[string]int `yaml:"to_map"`
	Dur     string         `yaml:"dur"`
}

type TempFadeEvent struct {
	Fixture string         `yaml:"fixture,omitempty"`
	ToMap   map[string]int `yaml:"to_map"`
	FadeIn  string         `yaml:"fade_in"`
	Hold    string         `yaml:"hold"`
	FadeOut string         `yaml:"fade_out"`
}

// ----- Utils -----

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func parseTimestampMS(s string) (int64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\uFEFF", ""))

	// Support plain seconds (e.g., "0.546554" or "12.5")
	if !strings.Contains(s, ":") {
		if sec, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(sec * 1000), nil
		}
		return 0, fmt.Errorf("bad time %q; must be numeric seconds or MM:SS.mmm", s)
	}

	// Supports "MM:SS.mmm" or "HH:MM:SS.mmm"
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("bad time %q; use MM:SS.mmm or HH:MM:SS.mmm or plain seconds", s)
	}
	var h, m int64
	var secMilli string
	if len(parts) == 3 {
		h64, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, err
		}
		h = h64
		m64, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, err
		}
		m = m64
		secMilli = parts[2]
	} else {
		h = 0
		m64, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, err
		}
		m = m64
		secMilli = parts[1]
	}
	var sPart int64
	var msPart int64
	if strings.Contains(secMilli, ".") {
		sp := strings.SplitN(secMilli, ".", 2)
		s64, err := strconv.ParseInt(sp[0], 10, 64)
		if err != nil {
			return 0, err
		}
		sPart = s64
		msStr := sp[1]
		if len(msStr) > 3 {
			msStr = msStr[:3] // truncate extra precision
		}
		for len(msStr) < 3 {
			msStr += "0"
		}
		ms64, err := strconv.ParseInt(msStr, 10, 64)
		if err != nil {
			return 0, err
		}
		msPart = ms64
	} else {
		s64, err := strconv.ParseInt(secMilli, 10, 64)
		if err != nil {
			return 0, err
		}
		sPart = s64
		msPart = 0
	}
	total := (((h*60 + m) * 60) + sPart) * 1000
	return total + msPart, nil
}

func decodeAudio(path string) (beep.StreamSeekCloser, beep.Format, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, beep.Format{}, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp3":
		s, format, err := mp3.Decode(f)
		if err != nil {
			f.Close()
			return nil, beep.Format{}, err
		}
		return s, format, nil
	case ".wav":
		s, format, err := wav.Decode(f)
		if err != nil {
			f.Close()
			return nil, beep.Format{}, err
		}
		return s, format, nil
	default:
		f.Close()
		return nil, beep.Format{}, fmt.Errorf("unsupported audio format %q (use mp3 or wav)", ext)
	}
}

// ----- Art-Net -----

type ArtNetSender struct {
	conn     net.Conn
	universe uint16
	seq      uint8
}

func NewArtNetSender(target string, universe uint16) (*ArtNetSender, error) {
	c, err := net.Dial("udp", net.JoinHostPort(target, "6454"))
	if err != nil {
		return nil, err
	}
	return &ArtNetSender{conn: c, universe: universe, seq: 1}, nil
}

func (a *ArtNetSender) Close() { _ = a.conn.Close() }

func (a *ArtNetSender) Send(dmx []byte) error {
	if len(dmx) > 512 {
		return errors.New("dmx length must be <= 512")
	}
	buf := buildArtDMX(a.seq, a.universe, dmx)
	a.seq++
	_, err := a.conn.Write(buf)
	return err
}

func buildArtDMX(seq uint8, universe uint16, dmx []byte) []byte {
	if dmx == nil {
		dmx = make([]byte, 512)
	}
	subUni := byte(universe & 0xFF)
	netHi := byte((universe >> 8) & 0x7F)

	pkt := make([]byte, 18+len(dmx))
	copy(pkt[0:], []byte("Art-Net\x00"))
	// OpCode (little-endian) = OpOutput/DMX (0x5000)
	pkt[8] = 0x00
	pkt[9] = 0x50
	// ProtVer
	pkt[10] = 0x00
	pkt[11] = 14
	// Sequence, Physical
	pkt[12] = seq
	pkt[13] = 0x00
	// SubUni (low), Net (high)
	pkt[14] = subUni
	pkt[15] = netHi
	// Length (big-endian)
	n := len(dmx)
	pkt[16] = byte((n >> 8) & 0xFF)
	pkt[17] = byte(n & 0xFF)
	copy(pkt[18:], dmx)
	return pkt
}

// ----- Scheduler -----

type compiledFade struct {
	ch      int // 1..512
	from    int
	to      int
	startMS int64
	endMS   int64
}

type compiledSet struct {
	ch    int
	value int
	atMS  int64
}

type compiledTempSet struct {
	fixture string
	attrs   map[string]int
	startMS int64
	endMS   int64
}

type compiledTempFade struct {
	fixture   string
	attrs     map[string]int
	startMS   int64
	fadeInMS  int64
	holdMS    int64
	fadeOutMS int64
}

type program struct {
	audioPath string
	offsetMS  int
	bpm       float64
	fades     []compiledFade
	sets      []compiledSet
	tempSets  []compiledTempSet
	tempFades []compiledTempFade
}

func compile(proj Project) (program, error) {
	var fades []compiledFade
	var sets []compiledSet
	var tempSets []compiledTempSet
	var tempFades []compiledTempFade

	for i := range proj.Events {
		ev := proj.Events[i]
		if ev.Set != nil {
			atMS, err := parseTimestampMS(ev.At)
			if err != nil {
				return program{}, fmt.Errorf("event %d: %w", i, err)
			}
			if len(ev.Set.Values) > 0 {
				for attr, val := range ev.Set.Values {
					ch, err := proj.resolveFixtureAttr(fmt.Sprintf("%s.%s", ev.Set.Fixture, attr), 0)
					if err != nil {
						return program{}, err
					}
					sets = append(sets, compiledSet{ch: ch, value: clamp(val, 0, 255), atMS: atMS})
				}
			} else {
				ch, err := proj.resolveFixtureAttr(ev.Set.Fixture, ev.Set.Ch)
				if err != nil {
					return program{}, err
				}
				sets = append(sets, compiledSet{ch: ch, value: clamp(ev.Set.Value, 0, 255), atMS: atMS})
			}
		} else if ev.Fade != nil {
			atMS, err := parseTimestampMS(ev.At)
			if err != nil {
				return program{}, fmt.Errorf("event %d: %w", i, err)
			}
			d, err := time.ParseDuration(ev.Fade.Dur)
			if err != nil {
				return program{}, fmt.Errorf("event %d fade dur: %w", i, err)
			}
			start := atMS
			end := atMS + d.Milliseconds()

			if len(ev.Fade.ToMap) > 0 {
				for attr, toVal := range ev.Fade.ToMap {
					var fromVal int
					if ev.Fade.FromMap != nil {
						fromVal = ev.Fade.FromMap[attr]
					}
					ch, err := proj.resolveFixtureAttr(fmt.Sprintf("%s.%s", ev.Fade.Fixture, attr), 0)
					if err != nil {
						return program{}, err
					}
					fades = append(fades, compiledFade{
						ch:      ch,
						from:    clamp(fromVal, 0, 255),
						to:      clamp(toVal, 0, 255),
						startMS: start,
						endMS:   end,
					})
				}
			} else {
				ch, err := proj.resolveFixtureAttr(ev.Fade.Fixture, ev.Fade.Ch)
				if err != nil {
					return program{}, err
				}
				fades = append(fades, compiledFade{
					ch:      ch,
					from:    clamp(ev.Fade.From, 0, 255),
					to:      clamp(ev.Fade.To, 0, 255),
					startMS: start,
					endMS:   end,
				})
			}
		} else if ev.TempSet != nil {
			atMS, err := parseTimestampMS(ev.At)
			if err != nil {
				return program{}, fmt.Errorf("event %d: %w", i, err)
			}
			dur, err := time.ParseDuration(ev.TempSet.Dur)
			if err != nil {
				return program{}, fmt.Errorf("event %d temp_set dur: %w", i, err)
			}
			end := atMS + dur.Milliseconds()
			tempSets = append(tempSets, compiledTempSet{
				fixture: ev.TempSet.Fixture,
				attrs:   ev.TempSet.ToMap,
				startMS: atMS,
				endMS:   end,
			})
		} else if ev.TempFade != nil {
			atMS, err := parseTimestampMS(ev.At)
			if err != nil {
				return program{}, fmt.Errorf("event %d: %w", i, err)
			}
			fadeIn, err := time.ParseDuration(ev.TempFade.FadeIn)
			if err != nil {
				return program{}, err
			}
			hold, err := time.ParseDuration(ev.TempFade.Hold)
			if err != nil {
				return program{}, err
			}
			fadeOut, err := time.ParseDuration(ev.TempFade.FadeOut)
			if err != nil {
				return program{}, err
			}
			tempFades = append(tempFades, compiledTempFade{
				fixture:   ev.TempFade.Fixture,
				attrs:     ev.TempFade.ToMap,
				startMS:   atMS,
				fadeInMS:  fadeIn.Milliseconds(),
				holdMS:    hold.Milliseconds(),
				fadeOutMS: fadeOut.Milliseconds(),
			})
		} else {
			return program{}, fmt.Errorf("event %d: must have set, fade, temp_set or temp_fade", i)
		}
	}

	sort.Slice(sets, func(i, j int) bool { return sets[i].atMS < sets[j].atMS })
	sort.Slice(fades, func(i, j int) bool { return fades[i].startMS < fades[j].startMS })
	sort.Slice(tempSets, func(i, j int) bool { return tempSets[i].startMS < tempSets[j].startMS })
	sort.Slice(tempFades, func(i, j int) bool { return tempFades[i].startMS < tempFades[j].startMS })

	return program{
		audioPath: proj.Audio,
		offsetMS:  proj.OffsetMS,
		bpm:       proj.BPM,
		fades:     fades,
		sets:      sets,
		tempSets:  tempSets,
		tempFades: tempFades,
	}, nil
}

// Resolve "fixture.attr" to absolute DMX channel, or fall back to explicit ch if fixtureAttr is empty.
func (p *Project) resolveFixtureAttr(fixtureAttr string, ch int) (int, error) {
	if fixtureAttr == "" {
		return ch, nil
	}
	parts := strings.SplitN(fixtureAttr, ".", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("fixture reference %q must be fixture.attr", fixtureAttr)
	}
	fxID, attr := parts[0], parts[1]

	// find patched fixture by ID
	var fx *PatchedFixture
	for i := range p.Patch {
		if p.Patch[i].ID == fxID {
			fx = &p.Patch[i]
			break
		}
	}
	if fx == nil {
		return 0, fmt.Errorf("unknown fixture %q", fxID)
	}

	prof, ok := p.Profiles[fx.Profile]
	if !ok {
		return 0, fmt.Errorf("unknown profile %q for fixture %q", fx.Profile, fxID)
	}
	rel, ok := prof.Channels[attr]
	if !ok {
		return 0, fmt.Errorf("profile %q has no channel for attr %q", fx.Profile, attr)
	}
	abs := fx.Base + rel - 1 // rel is 1-based
	if abs < 1 || abs > 512 {
		return 0, fmt.Errorf("resolved channel %d out of range 1..512", abs)
	}
	return abs, nil
}

func run(prog program, proj Project, art *ArtNetSender) error {
	stream, format, err := decodeAudio(prog.audioPath)
	if err != nil {
		return err
	}
	defer stream.Close()

	// Speaker init
	sr := format.SampleRate
	must(speaker.Init(sr, sr.N(time.Second/10))) // 100ms buffer

	// track start time
	started := make(chan struct{})
	var startWall time.Time

	// Wrap the streamer to capture start time precisely when audio starts
	wrapped := beep.StreamerFunc(func(samples [][2]float64) (n int, ok bool) {
		if startWall.IsZero() {
			startWall = time.Now()
			close(started)
		}
		return stream.Stream(samples)
	})

	speaker.Play(beep.Seq(wrapped, beep.Callback(func() {})))

	// Wait until the first audio callback fires
	<-started

	// DMX loop (~44 Hz)
	ticker := time.NewTicker(23 * time.Millisecond)
	defer ticker.Stop()

	var dmx [512]byte
	nextSetIdx := 0
	activeFades := prog.fades

	nextTempSetIdx := 0
	nextTempFadeIdx := 0

	for {
		select {
		case <-ticker.C:
			nowMS := time.Since(startWall).Milliseconds() + int64(prog.offsetMS)

			// Apply sets that are due
			for nextSetIdx < len(prog.sets) && prog.sets[nextSetIdx].atMS <= nowMS {
				ch := prog.sets[nextSetIdx].ch
				if ch >= 1 && ch <= 512 {
					dmx[ch-1] = byte(prog.sets[nextSetIdx].value)
				}
				nextSetIdx++
			}

			// Apply active fades
			if len(activeFades) > 0 {
				var remaining []compiledFade
				for _, f := range activeFades {
					if nowMS < f.startMS {
						remaining = append(remaining, f)
						continue
					}
					if nowMS >= f.endMS {
						// finished -> set final
						if f.ch >= 1 && f.ch <= 512 {
							dmx[f.ch-1] = byte(clamp(f.to, 0, 255))
						}
						continue
					}
					// interpolate
					if f.ch >= 1 && f.ch <= 512 {
						t := float64(nowMS-f.startMS) / float64(f.endMS-f.startMS)
						val := int(float64(f.from) + t*float64(f.to-f.from))
						dmx[f.ch-1] = byte(clamp(val, 0, 255))
					}
					remaining = append(remaining, f)
				}
				activeFades = remaining
			}

			// Handle temp_set
			for nextTempSetIdx < len(prog.tempSets) && prog.tempSets[nextTempSetIdx].startMS <= nowMS {
				ts := prog.tempSets[nextTempSetIdx]
				snapshot := make(map[string]byte)
				for attr := range ts.attrs {
					ch, _ := proj.resolveFixtureAttr(fmt.Sprintf("%s.%s", ts.fixture, attr), 0)
					if ch >= 1 && ch <= 512 {
						snapshot[attr] = dmx[ch-1]
						dmx[ch-1] = byte(clamp(ts.attrs[attr], 0, 255))
					}
				}
				go func(ts compiledTempSet, snap map[string]byte) {
					time.Sleep(time.Duration(ts.endMS-ts.startMS) * time.Millisecond)
					for attr, v := range snap {
						ch, _ := proj.resolveFixtureAttr(fmt.Sprintf("%s.%s", ts.fixture, attr), 0)
						if ch >= 1 && ch <= 512 {
							dmx[ch-1] = v
						}
					}
				}(ts, snapshot)
				nextTempSetIdx++
			}

			// Handle temp_fade
			for nextTempFadeIdx < len(prog.tempFades) && prog.tempFades[nextTempFadeIdx].startMS <= nowMS {
				tf := prog.tempFades[nextTempFadeIdx]
				snapshot := make(map[string]byte)
				for attr := range tf.attrs {
					ch, _ := proj.resolveFixtureAttr(fmt.Sprintf("%s.%s", tf.fixture, attr), 0)
					if ch >= 1 && ch <= 512 {
						snapshot[attr] = dmx[ch-1]
					}
				}
				go func(tf compiledTempFade, snap map[string]byte) {
					// fade in
					fadeDur := time.Duration(tf.fadeInMS) * time.Millisecond
					start := time.Now()
					for time.Since(start) < fadeDur {
						t := float64(time.Since(start).Milliseconds()) / float64(tf.fadeInMS)
						for attr, toVal := range tf.attrs {
							ch, _ := proj.resolveFixtureAttr(fmt.Sprintf("%s.%s", tf.fixture, attr), 0)
							if ch >= 1 && ch <= 512 {
								from := float64(snap[attr])
								val := int(from + t*(float64(toVal)-from))
								dmx[ch-1] = byte(clamp(val, 0, 255))
							}
						}
						time.Sleep(23 * time.Millisecond)
					}
					// hold
					time.Sleep(time.Duration(tf.holdMS) * time.Millisecond)
					// fade out
					fadeOutDur := time.Duration(tf.fadeOutMS) * time.Millisecond
					start = time.Now()
					for time.Since(start) < fadeOutDur {
						t := float64(time.Since(start).Milliseconds()) / float64(tf.fadeOutMS)
						for attr := range tf.attrs {
							ch, _ := proj.resolveFixtureAttr(fmt.Sprintf("%s.%s", tf.fixture, attr), 0)
							if ch >= 1 && ch <= 512 {
								from := float64(tf.attrs[attr])
								to := float64(snap[attr])
								val := int(from + t*(to-from))
								dmx[ch-1] = byte(clamp(val, 0, 255))
							}
						}
						time.Sleep(23 * time.Millisecond)
					}
				}(tf, snapshot)
				nextTempFadeIdx++
			}

			// Send frame
			if err := art.Send(dmx[:]); err != nil {
				return err
			}

			// stop when the audio stream finishes
			if stream.Position() >= stream.Len() {
				time.Sleep(3 * time.Second) // optional DMX hold
				return nil
			}
		}
	}
}

func guessTotalDurationMS(p program) int64 {
	// crude: last of endMS or set atMS + 5s
	last := int64(0)
	for _, f := range p.fades {
		if f.endMS > last {
			last = f.endMS
		}
	}
	for _, s := range p.sets {
		if s.atMS > last {
			last = s.atMS
		}
	}
	if last == 0 {
		last = 5000
	}
	return last
}

// ----- Main -----

func usage() {
	fmt.Println("Usage: dmx_go <project.yaml>")
}

func main() {
	if len(os.Args) != 2 {
		usage()
		os.Exit(1)
	}

	yamlBytes, err := os.ReadFile(os.Args[1])
	must(err)

	dec := yaml.NewDecoder(bytes.NewReader(yamlBytes))
	dec.KnownFields(true)

	var proj Project
	must(dec.Decode(&proj))
	if proj.Artnet.Target == "" || proj.Audio == "" {
		must(errors.New("YAML must include artnet.target and audio"))
	}

	prog, err := compile(proj)
	must(err)

	sender, err := NewArtNetSender(proj.Artnet.Target, proj.Artnet.Universe)
	must(err)
	defer sender.Close()

	fmt.Printf("Playing %s, unicast to %s (universe %d), offset %dms...\n",
		filepath.Base(prog.audioPath), proj.Artnet.Target, proj.Artnet.Universe, prog.offsetMS)

	must(run(prog, proj, sender))
	fmt.Println("Done.")
}

// Ensure imports not optimized away
var _ io.Closer
