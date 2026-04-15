package announce

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Header represents a single HTTP request header name/value pair.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// keyAlgorithmConfig holds the algorithm configuration for key generation.
type keyAlgorithmConfig struct {
	Type   string `json:"type"`
	Length int    `json:"length"`
}

// keyGeneratorConfig holds the full key generator settings.
type keyGeneratorConfig struct {
	Algorithm    keyAlgorithmConfig `json:"algorithm"`
	RefreshOn    string             `json:"refreshOn"`
	RefreshEvery int                `json:"refreshEvery"`
	KeyCase      string             `json:"keyCase"`
}

// peerIDAlgorithmConfig holds the algorithm configuration for peer ID generation.
type peerIDAlgorithmConfig struct {
	Type    string `json:"type"`
	Pattern string `json:"pattern"`
	Length  int    `json:"length"`
}

// peerIDGeneratorConfig holds the full peer ID generator settings.
type peerIDGeneratorConfig struct {
	Algorithm     peerIDAlgorithmConfig `json:"algorithm"`
	RefreshOn     string                `json:"refreshOn"`
	ShouldURLEncode bool               `json:"shouldUrlEncode"`
}

// urlEncoderConfig holds URL encoding settings.
type urlEncoderConfig struct {
	EncodingExclusionPattern string `json:"encodingExclusionPattern"`
	EncodedHexCase           string `json:"encodedHexCase"`
}

// rawClientFile is the JSON structure of a .client file.
type rawClientFile struct {
	KeyGenerator    keyGeneratorConfig    `json:"keyGenerator"`
	PeerIDGenerator peerIDGeneratorConfig `json:"peerIdGenerator"`
	URLEncoder      urlEncoderConfig      `json:"urlEncoder"`
	Query           string                `json:"query"`
	Numwant         int                   `json:"numwant"`
	NumwantOnStop   int                   `json:"numwantOnStop"`
	RequestHeaders  []Header              `json:"requestHeaders"`
}

// placeholderRE matches any unknown {placeholder} in query templates.
var placeholderRE = regexp.MustCompile(`\{[^}]+\}`)

// ClientConfig holds the parsed and initialized client configuration.
type ClientConfig struct {
	Query          string
	Numwant        int
	NumwantOnStop  int
	RequestHeaders []Header
	PeerID         string
	Key            string
	UserAgent      string

	// internal fields for key refresh logic
	keyGen        keyGeneratorConfig
	peerIDGen     peerIDGeneratorConfig
	urlEncoder    urlEncoderConfig
	announceCount int
	exclusionRE   *regexp.Regexp
}

// AnnounceParams holds the per-announce variable parameters.
type AnnounceParams struct {
	InfoHash   [20]byte
	Port       int
	Uploaded   int64
	Downloaded int64
	Left       int64
	Event      string // "started", "stopped", "completed", ""
	IP         string // optional: override IP reported to tracker
}

// LoadClientConfig parses a .client JSON file, generates the initial PeerID
// and Key, and returns a ready-to-use ClientConfig.
func LoadClientConfig(path string) (*ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("client: reading %q: %w", path, err)
	}

	var raw rawClientFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("client: parsing %q: %w", path, err)
	}

	peerID, err := generatePeerID(raw.PeerIDGenerator)
	if err != nil {
		return nil, fmt.Errorf("client: generating peer ID: %w", err)
	}

	key, err := generateKey(raw.KeyGenerator)
	if err != nil {
		return nil, fmt.Errorf("client: generating key: %w", err)
	}

	userAgent := extractUserAgent(raw.RequestHeaders)

	return &ClientConfig{
		Query:          raw.Query,
		Numwant:        raw.Numwant,
		NumwantOnStop:  raw.NumwantOnStop,
		RequestHeaders: raw.RequestHeaders,
		PeerID:         peerID,
		Key:            key,
		UserAgent:      userAgent,
		keyGen:         raw.KeyGenerator,
		peerIDGen:      raw.PeerIDGenerator,
		urlEncoder:     raw.URLEncoder,
		exclusionRE:    compileExclusion(raw.URLEncoder.EncodingExclusionPattern),
	}, nil
}

// BuildAnnounceURL constructs the full announce URL by substituting all
// placeholders in the query template and appending it to announceURL.
func (c *ClientConfig) BuildAnnounceURL(announceURL string, params AnnounceParams) string {
	c.maybeRefreshKey(params.Event)

	numwant := c.Numwant
	if params.Event == "stopped" {
		numwant = c.NumwantOnStop
	}

	infoHashEncoded := encodeBytesRE(params.InfoHash[:], c.exclusionRE, c.urlEncoder.EncodedHexCase)
	peerIDEncoded := encodePeerID(c.PeerID, c.peerIDGen, c.urlEncoder)

	query := c.Query
	query = strings.ReplaceAll(query, "{infohash}", infoHashEncoded)
	query = strings.ReplaceAll(query, "{peerid}", peerIDEncoded)
	query = strings.ReplaceAll(query, "{port}", strconv.Itoa(params.Port))
	query = strings.ReplaceAll(query, "{uploaded}", strconv.FormatInt(params.Uploaded, 10))
	query = strings.ReplaceAll(query, "{downloaded}", strconv.FormatInt(params.Downloaded, 10))
	query = strings.ReplaceAll(query, "{left}", strconv.FormatInt(params.Left, 10))
	query = strings.ReplaceAll(query, "{key}", c.Key)
	query = strings.ReplaceAll(query, "{event}", params.Event)
	query = strings.ReplaceAll(query, "{numwant}", strconv.Itoa(numwant))
	// Remove unknown placeholders like {ipv6}, {locale}
	query = placeholderRE.ReplaceAllString(query, "")

	// Strip trailing & that may result from removed placeholders like &ipv6=
	query = strings.TrimRight(query, "&")
	// Collapse double-ampersands
	for strings.Contains(query, "&&") {
		query = strings.ReplaceAll(query, "&&", "&")
	}

	if params.IP != "" {
		query += "&ip=" + url.QueryEscape(params.IP)
	}

	c.announceCount++

	sep := "?"
	if strings.Contains(announceURL, "?") {
		sep = "&"
	}
	return announceURL + sep + query
}

// maybeRefreshKey refreshes the key according to the refresh policy.
func (c *ClientConfig) maybeRefreshKey(event string) {
	switch c.keyGen.RefreshOn {
	case "TIMED_OR_AFTER_STARTED_ANNOUNCE":
		if event == "started" {
			key, err := generateKey(c.keyGen)
			if err == nil {
				c.Key = key
			}
			return
		}
		if c.keyGen.RefreshEvery > 0 && c.announceCount > 0 && c.announceCount%c.keyGen.RefreshEvery == 0 {
			key, err := generateKey(c.keyGen)
			if err == nil {
				c.Key = key
			}
		}
	case "NEVER", "TORRENT_PERSISTENT":
		// no-op: key stays constant for the session
	}
}

// generateKey produces a random hex key according to the key generator config.
func generateKey(cfg keyGeneratorConfig) (string, error) {
	length := cfg.Algorithm.Length
	if length <= 0 {
		length = 8
	}

	switch cfg.Algorithm.Type {
	case "HASH", "HASH_NO_LEADING_ZERO":
		buf := make([]byte, (length+1)/2)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		key := hex.EncodeToString(buf)[:length]

		if cfg.Algorithm.Type == "HASH_NO_LEADING_ZERO" && len(key) > 0 && key[0] == '0' {
			// Replace leading zero with a random non-zero hex digit.
			b := make([]byte, 1)
			for {
				if _, err := rand.Read(b); err != nil {
					return "", fmt.Errorf("reading random byte: %w", err)
				}
				digit := b[0] & 0x0f
				if digit != 0 {
					key = fmt.Sprintf("%x", digit) + key[1:]
					break
				}
			}
		}

		if strings.EqualFold(cfg.KeyCase, "upper") {
			key = strings.ToUpper(key)
		}
		return key, nil

	default:
		// Unknown algorithm — fall back to random hex.
		buf := make([]byte, (length+1)/2)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		return hex.EncodeToString(buf)[:length], nil
	}
}

// generatePeerID produces a peer ID according to the peer ID generator config.
func generatePeerID(cfg peerIDGeneratorConfig) (string, error) {
	switch cfg.Algorithm.Type {
	case "REGEX":
		return generatePeerIDFromRegex(cfg.Algorithm.Pattern)
	case "HASH":
		length := cfg.Algorithm.Length
		if length <= 0 {
			length = 20
		}
		buf := make([]byte, length)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		return string(buf), nil
	default:
		buf := make([]byte, 20)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		return string(buf), nil
	}
}

// generatePeerIDFromRegex interprets a simplified peer ID pattern.
// It extracts any leading ASCII prefix (e.g., "-UT3220-To") and fills the
// remaining bytes with random printable characters drawn from the pattern's
// character classes, falling back to alphanumeric characters.
func generatePeerIDFromRegex(pattern string) (string, error) {
	const totalLen = 20

	// Extract literal ASCII prefix: everything up to the first '[', '(', or '\'
	prefixEnd := strings.IndexAny(pattern, `[(\`)
	prefix := ""
	if prefixEnd < 0 {
		prefix = pattern
	} else {
		prefix = pattern[:prefixEnd]
	}

	// Keep only printable ASCII from the prefix (some .client files embed
	// raw high-bytes as decorators between literal segments).
	var asciiPrefix strings.Builder
	for _, r := range prefix {
		if r >= 0x20 && r <= 0x7e {
			asciiPrefix.WriteRune(r)
		}
	}
	prefix = asciiPrefix.String()
	if len(prefix) > totalLen {
		prefix = prefix[:totalLen]
	}

	remaining := totalLen - len(prefix)
	if remaining <= 0 {
		return prefix, nil
	}

	// Determine fill characters from what the pattern allows after the prefix.
	// Default to alphanumeric if the pattern isn't parseable.
	fillChars := buildFillChars(pattern[prefixEnd:])
	if len(fillChars) == 0 {
		fillChars = []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")
	}

	buf := make([]byte, remaining)
	randBuf := make([]byte, remaining)
	if _, err := rand.Read(randBuf); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	for i := range buf {
		buf[i] = fillChars[int(randBuf[i])%len(fillChars)]
	}

	return prefix + string(buf), nil
}

// buildFillChars extracts a character set from the tail of a regex pattern.
// It looks for the first character class [...] or range \x01-\xff and returns
// the matching byte slice.
func buildFillChars(tail string) []byte {
	if tail == "" {
		return nil
	}

	// Look for a character class like [A-Za-z0-9_~\(\)!\.\*-]
	start := strings.Index(tail, "[")
	end := strings.Index(tail, "]")
	if start >= 0 && end > start {
		classStr := tail[start+1 : end]
		return expandCharClass(classStr)
	}

	// Look for raw byte range like \u0001-\u00ff (already decoded as runes).
	// In this case just use printable ASCII.
	return []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")
}

// expandCharClass expands a character class string (contents between [ and ])
// into the set of bytes it represents.
func expandCharClass(class string) []byte {
	var chars []byte
	seen := make(map[byte]bool)

	add := func(b byte) {
		if !seen[b] {
			seen[b] = true
			chars = append(chars, b)
		}
	}

	runes := []rune(class)
	i := 0
	for i < len(runes) {
		if runes[i] > 0x7e {
			// Skip non-ASCII rune literals that appear in some .client files.
			i++
			continue
		}
		if i+2 < len(runes) && runes[i+1] == '-' && runes[i+2] > runes[i] && runes[i+2] <= 0x7e {
			// Range: a-z
			for b := byte(runes[i]); b <= byte(runes[i+2]); b++ {
				add(b)
			}
			i += 3
			continue
		}
		if runes[i] == '\\' && i+1 < len(runes) {
			// Escaped character: \( \) \! etc.
			i++
			if runes[i] <= 0x7e {
				add(byte(runes[i]))
			}
			i++
			continue
		}
		if runes[i] <= 0x7e {
			add(byte(runes[i]))
		}
		i++
	}

	return chars
}

// encodeBytesRE percent-encodes a byte slice using a pre-compiled exclusion regex.
func encodeBytesRE(b []byte, exclusion *regexp.Regexp, hexCase string) string {
	var sb strings.Builder
	for _, bt := range b {
		if exclusion != nil && exclusion.MatchString(string([]byte{bt})) {
			sb.WriteByte(bt)
		} else {
			if strings.EqualFold(hexCase, "upper") {
				fmt.Fprintf(&sb, "%%%02X", bt)
			} else {
				fmt.Fprintf(&sb, "%%%02x", bt)
			}
		}
	}
	return sb.String()
}

// encodeBytes percent-encodes a byte slice, skipping bytes matching the
// exclusion pattern from the client's urlEncoder config.
func encodeBytes(b []byte, enc urlEncoderConfig) string {
	return encodeBytesRE(b, compileExclusion(enc.EncodingExclusionPattern), enc.EncodedHexCase)
}

// encodePeerID percent-encodes the peer ID string using the client URL encoder
// when shouldUrlEncode is true, otherwise returns it raw.
func encodePeerID(peerID string, gen peerIDGeneratorConfig, enc urlEncoderConfig) string {
	if gen.ShouldURLEncode {
		return encodeBytes([]byte(peerID), enc)
	}
	return url.QueryEscape(peerID)
}

// compileExclusion compiles the exclusion regex, caching nothing (called once
// per announce). Returns nil on error.
func compileExclusion(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return re
}

// extractUserAgent finds the User-Agent header value from the header list.
func extractUserAgent(headers []Header) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, "user-agent") {
			return h.Value
		}
	}
	return ""
}
