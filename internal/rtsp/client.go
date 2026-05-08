package rtsp

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	mch264 "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	mch265 "github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/pion/rtp"
)

// VideoCodec identifies which H.26x variant is currently being pulled.
type VideoCodec int

const (
	CodecH264 VideoCodec = iota
	CodecH265
)

func (c VideoCodec) String() string {
	switch c {
	case CodecH264:
		return "H264"
	case CodecH265:
		return "H265"
	}
	return "unknown"
}

// Audio MIME types we can forward to browsers directly (no transcode).
const (
	AudioMimePCMU = "audio/PCMU"
	AudioMimePCMA = "audio/PCMA"
	AudioMimeOpus = "audio/opus"
)

// AudioFormat describes the audio track exposed by the camera, if any.
// Only codecs supported natively by browser WebRTC are ever populated here;
// AAC and friends are skipped.
type AudioFormat struct {
	MimeType     string
	ClockRate    uint32
	ChannelCount uint16
	SDPFmtpLine  string
}

// AudioSink receives raw RTP packets from the camera's audio track. Must not
// block.
type AudioSink interface {
	PushRTP(pkt *rtp.Packet)
}

// AccessUnit is a decoded H.264/H.265 access unit (one frame's NALUs) with a
// PTS expressed in the video track's clock rate (typically 90 kHz).
type AccessUnit struct {
	NALUs [][]byte
	PTS   time.Duration
	IDR   bool
}

func ptsToDuration(pts int64, clockRate int) time.Duration {
	if clockRate <= 0 {
		return 0
	}
	return time.Duration(pts) * time.Second / time.Duration(clockRate)
}

// Sink is anything that wants to receive decoded video access units.
// Implementations must not block; they should drop or buffer as needed.
type Sink interface {
	PushAU(au AccessUnit)
}

// Client is an auto-reconnecting RTSP pull client that fans out H.264 or
// H.265 access units to registered sinks.
type Client struct {
	mu        sync.Mutex
	url       string
	cancel    context.CancelFunc
	running   bool
	connected atomic.Bool

	sinksMu sync.RWMutex
	sinks   map[int]Sink
	nextID  int

	// latest parameter sets observed from SDP or in-band. VPS is only used
	// for H.265. Sent to new sinks before the next IDR frame so players
	// can initialize.
	paramMu sync.RWMutex
	codec   VideoCodec
	vps     []byte
	sps     []byte
	pps     []byte

	// audio track info + fan-out. audioFmt is nil until a usable audio format
	// is discovered on the current (or a previous) connect.
	audioMu  sync.RWMutex
	audioFmt *AudioFormat

	audioSinksMu sync.RWMutex
	audioSinks   map[int]AudioSink
	audioNextID  int
}

func NewClient() *Client {
	return &Client{
		sinks:      make(map[int]Sink),
		audioSinks: make(map[int]AudioSink),
	}
}

func (c *Client) SetURL(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.url == url && c.running {
		return
	}
	c.url = url
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if url == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.running = true
	go c.loop(ctx, url)
}

func (c *Client) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.running = false
	c.connected.Store(false)
}

func (c *Client) Connected() bool {
	return c.connected.Load()
}

// Codec returns the video codec currently being received, or CodecH264 if
// no stream has been set up yet.
func (c *Client) Codec() VideoCodec {
	c.paramMu.RLock()
	defer c.paramMu.RUnlock()
	return c.codec
}

// Parameters returns the parameter-set NALUs in canonical order
// (VPS, SPS, PPS for H.265; SPS, PPS for H.264). Missing ones are omitted.
func (c *Client) Parameters() [][]byte {
	c.paramMu.RLock()
	defer c.paramMu.RUnlock()
	var out [][]byte
	if c.codec == CodecH265 && c.vps != nil {
		out = append(out, cloneBytes(c.vps))
	}
	if c.sps != nil {
		out = append(out, cloneBytes(c.sps))
	}
	if c.pps != nil {
		out = append(out, cloneBytes(c.pps))
	}
	return out
}

// AddSink registers a sink. Returns an ID that can be passed to RemoveSink.
func (c *Client) AddSink(s Sink) int {
	c.sinksMu.Lock()
	defer c.sinksMu.Unlock()
	c.nextID++
	id := c.nextID
	c.sinks[id] = s
	return id
}

func (c *Client) RemoveSink(id int) {
	c.sinksMu.Lock()
	defer c.sinksMu.Unlock()
	delete(c.sinks, id)
}

func (c *Client) fanOut(au AccessUnit) {
	c.sinksMu.RLock()
	defer c.sinksMu.RUnlock()
	for _, s := range c.sinks {
		s.PushAU(au)
	}
}

// AudioFormat returns a copy of the current audio format, or nil if none has
// been discovered yet or the camera lacks a browser-compatible audio track.
func (c *Client) AudioFormat() *AudioFormat {
	c.audioMu.RLock()
	defer c.audioMu.RUnlock()
	if c.audioFmt == nil {
		return nil
	}
	f := *c.audioFmt
	return &f
}

// AddAudioSink registers an audio sink. Returns an ID for RemoveAudioSink.
func (c *Client) AddAudioSink(s AudioSink) int {
	c.audioSinksMu.Lock()
	defer c.audioSinksMu.Unlock()
	c.audioNextID++
	id := c.audioNextID
	c.audioSinks[id] = s
	return id
}

func (c *Client) RemoveAudioSink(id int) {
	c.audioSinksMu.Lock()
	defer c.audioSinksMu.Unlock()
	delete(c.audioSinks, id)
}

func (c *Client) fanOutAudio(pkt *rtp.Packet) {
	c.audioSinksMu.RLock()
	defer c.audioSinksMu.RUnlock()
	for _, s := range c.audioSinks {
		s.PushRTP(pkt)
	}
}

func (c *Client) loop(ctx context.Context, url string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.runOnce(ctx, url)
		c.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("rtsp: %v; reconnecting in %v", err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) runOnce(ctx context.Context, rawURL string) error {
	u, err := base.ParseURL(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}

	client := gortsplib.Client{
		Scheme:       u.Scheme,
		Host:         u.Host,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	if err := client.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer client.Close()

	desc, _, err := client.Describe(u)
	if err != nil {
		return fmt.Errorf("describe: %w", err)
	}

	if err := c.setupVideo(&client, desc); err != nil {
		return err
	}

	c.setupAudio(&client, desc)

	if _, err := client.Play(nil); err != nil {
		return fmt.Errorf("play: %w", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- client.Wait() }()

	select {
	case <-ctx.Done():
		client.Close()
		<-waitErr
		return ctx.Err()
	case err := <-waitErr:
		return err
	}
}

// setupVideo picks the first H.264 or H.265 track in the SDP and wires it up.
// H.264 is preferred if both are present.
func (c *Client) setupVideo(client *gortsplib.Client, desc *description.Session) error {
	var h264Forma *format.H264
	if medi := desc.FindFormat(&h264Forma); medi != nil {
		return c.setupH264(client, desc, medi, h264Forma)
	}
	var h265Forma *format.H265
	if medi := desc.FindFormat(&h265Forma); medi != nil {
		return c.setupH265(client, desc, medi, h265Forma)
	}
	return fmt.Errorf("no H.264 or H.265 stream found")
}

func (c *Client) setupH264(client *gortsplib.Client, desc *description.Session, medi *description.Media, forma *format.H264) error {
	c.paramMu.Lock()
	c.codec = CodecH264
	c.vps = nil
	if forma.SPS != nil {
		c.sps = cloneBytes(forma.SPS)
	}
	if forma.PPS != nil {
		c.pps = cloneBytes(forma.PPS)
	}
	c.paramMu.Unlock()

	rtpDec, err := forma.CreateDecoder()
	if err != nil {
		return fmt.Errorf("create decoder: %w", err)
	}
	if _, err := client.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return fmt.Errorf("setup: %w", err)
	}

	gotIDR := false
	clockRate := forma.ClockRate()
	client.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		ptsRaw, ok := client.PacketPTS(medi, pkt)
		if !ok {
			return
		}
		pts := ptsToDuration(ptsRaw, clockRate)
		nalus, err := rtpDec.Decode(pkt)
		if err != nil {
			if err != rtph264.ErrNonStartingPacketAndNoPrevious && err != rtph264.ErrMorePacketsNeeded {
				log.Printf("rtsp: decode: %v", err)
			}
			return
		}
		c.connected.Store(true)

		for _, n := range nalus {
			if len(n) == 0 {
				continue
			}
			switch mch264.NALUType(n[0] & 0x1F) {
			case mch264.NALUTypeSPS:
				c.paramMu.Lock()
				c.sps = cloneBytes(n)
				c.paramMu.Unlock()
			case mch264.NALUTypePPS:
				c.paramMu.Lock()
				c.pps = cloneBytes(n)
				c.paramMu.Unlock()
			}
		}

		isIDR := mch264.IsRandomAccess(nalus)
		if !gotIDR && !isIDR {
			return
		}
		gotIDR = true

		c.fanOut(AccessUnit{NALUs: nalus, PTS: pts, IDR: isIDR})
	})
	log.Printf("rtsp: video track: H.264")
	return nil
}

func (c *Client) setupH265(client *gortsplib.Client, desc *description.Session, medi *description.Media, forma *format.H265) error {
	c.paramMu.Lock()
	c.codec = CodecH265
	if forma.VPS != nil {
		c.vps = cloneBytes(forma.VPS)
	}
	if forma.SPS != nil {
		c.sps = cloneBytes(forma.SPS)
	}
	if forma.PPS != nil {
		c.pps = cloneBytes(forma.PPS)
	}
	c.paramMu.Unlock()

	rtpDec, err := forma.CreateDecoder()
	if err != nil {
		return fmt.Errorf("create decoder: %w", err)
	}
	if _, err := client.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return fmt.Errorf("setup: %w", err)
	}

	gotIDR := false
	clockRate := forma.ClockRate()
	client.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		ptsRaw, ok := client.PacketPTS(medi, pkt)
		if !ok {
			return
		}
		pts := ptsToDuration(ptsRaw, clockRate)
		nalus, err := rtpDec.Decode(pkt)
		if err != nil {
			if err != rtph265.ErrNonStartingPacketAndNoPrevious && err != rtph265.ErrMorePacketsNeeded {
				log.Printf("rtsp: decode: %v", err)
			}
			return
		}
		c.connected.Store(true)

		for _, n := range nalus {
			if len(n) == 0 {
				continue
			}
			switch mch265.NALUType((n[0] >> 1) & 0x3F) {
			case mch265.NALUType_VPS_NUT:
				c.paramMu.Lock()
				c.vps = cloneBytes(n)
				c.paramMu.Unlock()
			case mch265.NALUType_SPS_NUT:
				c.paramMu.Lock()
				c.sps = cloneBytes(n)
				c.paramMu.Unlock()
			case mch265.NALUType_PPS_NUT:
				c.paramMu.Lock()
				c.pps = cloneBytes(n)
				c.paramMu.Unlock()
			}
		}

		isIDR := mch265.IsRandomAccess(nalus)
		if !gotIDR && !isIDR {
			return
		}
		gotIDR = true

		c.fanOut(AccessUnit{NALUs: nalus, PTS: pts, IDR: isIDR})
	})
	log.Printf("rtsp: video track: H.265")
	return nil
}

// setupAudio looks for the first browser-compatible audio track (G.711 µ-law,
// G.711 A-law, or Opus) and, if found, calls Setup and registers an RTP
// callback that fans packets out to audio sinks. Audio is best-effort —
// anything that fails here is logged and ignored.
func (c *Client) setupAudio(client *gortsplib.Client, desc *description.Session) {
	var audioMedi *description.Media
	var audioForma format.Format
	for _, m := range desc.Medias {
		if m.Type != description.MediaTypeAudio || m.IsBackChannel {
			continue
		}
		for _, f := range m.Formats {
			switch f.(type) {
			case *format.G711, *format.Opus:
				audioMedi = m
				audioForma = f
			}
			if audioForma != nil {
				break
			}
		}
		if audioForma != nil {
			break
		}
	}

	if audioForma == nil {
		c.audioMu.Lock()
		c.audioFmt = nil
		c.audioMu.Unlock()
		return
	}

	af := &AudioFormat{ClockRate: uint32(audioForma.ClockRate())}
	switch f := audioForma.(type) {
	case *format.G711:
		if f.MULaw {
			af.MimeType = AudioMimePCMU
		} else {
			af.MimeType = AudioMimePCMA
		}
		af.ChannelCount = uint16(f.ChannelCount)
	case *format.Opus:
		af.MimeType = AudioMimeOpus
		// RFC7587: rtpmap MUST be 48000/2 regardless of actual channel count.
		af.ClockRate = 48000
		af.ChannelCount = 2
		af.SDPFmtpLine = "minptime=10;useinbandfec=1"
	}

	if _, err := client.Setup(desc.BaseURL, audioMedi, 0, 0); err != nil {
		log.Printf("rtsp: audio setup: %v (continuing without audio)", err)
		c.audioMu.Lock()
		c.audioFmt = nil
		c.audioMu.Unlock()
		return
	}

	c.audioMu.Lock()
	c.audioFmt = af
	c.audioMu.Unlock()

	client.OnPacketRTP(audioMedi, audioForma, func(pkt *rtp.Packet) {
		c.fanOutAudio(pkt)
	})
	log.Printf("rtsp: audio track: %s %dHz %dch", af.MimeType, af.ClockRate, af.ChannelCount)
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
