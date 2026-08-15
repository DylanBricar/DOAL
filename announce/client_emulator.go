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
	Type           string `json:"type"`
	Pattern        string `json:"pattern"`
	Length         int    `json:"length"`
	Prefix         string `json:"prefix"`
	CharactersPool string `json:"charactersPool"`
	Base           int    `json:"base"`
}

// peerIDGeneratorConfig holds the full peer ID generator settings.
type peerIDGeneratorConfig struct {
	Algorithm       peerIDAlgorithmConfig `json:"algorithm"`
	RefreshOn       string                `json:"refreshOn"`
	ShouldURLEncode bool                  `json:"shouldUrlEncode"`
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

// CloneWithFreshIdentity copies a client profile and generates a distinct
// peer ID and tracker key while preserving its wire-format settings.
func (c *ClientConfig) CloneWithFreshIdentity() (*ClientConfig, error) {
	clone := c.clone()

	var err error
	for attempt := 0; attempt < 8; attempt++ {
		clone.PeerID, err = generatePeerID(c.peerIDGen)
		if err != nil {
			return nil, fmt.Errorf("generating cloned peer ID: %w", err)
		}
		if clone.PeerID != c.PeerID {
			break
		}
	}
	if clone.PeerID == c.PeerID {
		return nil, fmt.Errorf("could not generate a distinct cloned peer ID")
	}

	for attempt := 0; attempt < 8; attempt++ {
		clone.Key, err = generateKey(c.keyGen)
		if err != nil {
			return nil, fmt.Errorf("generating cloned tracker key: %w", err)
		}
		if clone.Key != c.Key {
			break
		}
	}
	if clone.Key == c.Key {
		return nil, fmt.Errorf("could not generate a distinct cloned tracker key")
	}
	return clone, nil
}

func (c *ClientConfig) clone() *ClientConfig {
	clone := *c
	clone.RequestHeaders = append([]Header(nil), c.RequestHeaders...)
	clone.announceCount = 0
	return &clone
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
	peerIDEncoded := encodePeerID(c.PeerID, c.peerIDGen, c.exclusionRE, c.urlEncoder.EncodedHexCase)

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
	case "RANDOM_POOL_WITH_CHECKSUM":
		return generateChecksummedPeerID(cfg.Algorithm)
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
	runes := []rune(pattern)
	peerID := make([]byte, 0, totalLen)

	for i := 0; i < len(runes); {
		switch runes[i] {
		case '[':
			end := i + 1
			for end < len(runes) && runes[end] != ']' {
				end++
			}
			if end >= len(runes) {
				return "", fmt.Errorf("unterminated peer ID character class")
			}
			pool := expandCharClass(string(runes[i+1 : end]))
			if len(pool) == 0 {
				return "", fmt.Errorf("empty peer ID character class")
			}
			count := 1
			next := end + 1
			if next < len(runes) && runes[next] == '{' {
				closeAt := next + 1
				for closeAt < len(runes) && runes[closeAt] != '}' {
					closeAt++
				}
				if closeAt >= len(runes) {
					return "", fmt.Errorf("unterminated peer ID quantifier")
				}
				parsed, err := strconv.Atoi(string(runes[next+1 : closeAt]))
				if err != nil || parsed < 0 {
					return "", fmt.Errorf("invalid peer ID quantifier %q", string(runes[next:closeAt+1]))
				}
				count = parsed
				next = closeAt + 1
			}
			randomBytes := make([]byte, count)
			if _, err := rand.Read(randomBytes); err != nil {
				return "", fmt.Errorf("reading random bytes: %w", err)
			}
			for _, randomByte := range randomBytes {
				peerID = append(peerID, pool[int(randomByte)%len(pool)])
			}
			i = next
		case '(':
			end := i + 1
			for end < len(runes) && runes[end] != ')' {
				end++
			}
			if end >= len(runes) {
				return "", fmt.Errorf("unterminated peer ID literal group")
			}
			for _, literal := range runes[i+1 : end] {
				if literal > 0xff {
					return "", fmt.Errorf("peer ID literal U+%04X exceeds one byte", literal)
				}
				peerID = append(peerID, byte(literal))
			}
			i = end + 1
		case '\\':
			if i+1 >= len(runes) || runes[i+1] > 0xff {
				return "", fmt.Errorf("invalid escaped peer ID literal")
			}
			peerID = append(peerID, byte(runes[i+1]))
			i += 2
		default:
			if runes[i] > 0xff {
				return "", fmt.Errorf("peer ID literal U+%04X exceeds one byte", runes[i])
			}
			peerID = append(peerID, byte(runes[i]))
			i++
		}
	}

	if len(peerID) != totalLen {
		return "", fmt.Errorf("generated peer ID has %d bytes, want %d", len(peerID), totalLen)
	}
	return string(peerID), nil
}

func generateChecksummedPeerID(cfg peerIDAlgorithmConfig) (string, error) {
	const totalLen = 20
	suffixLength := totalLen - len(cfg.Prefix)
	if cfg.Prefix == "" || suffixLength < 2 {
		return "", fmt.Errorf("invalid checksummed peer ID prefix %q", cfg.Prefix)
	}
	if cfg.Base <= 0 || cfg.Base > len(cfg.CharactersPool) {
		return "", fmt.Errorf("invalid checksummed peer ID base %d", cfg.Base)
	}

	randomBytes := make([]byte, suffixLength-1)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	suffix := make([]byte, suffixLength)
	total := 0
	for i, randomByte := range randomBytes {
		value := int(randomByte) % cfg.Base
		total += value
		suffix[i] = cfg.CharactersPool[value]
	}
	checksum := (cfg.Base - total%cfg.Base) % cfg.Base
	suffix[len(suffix)-1] = cfg.CharactersPool[checksum]
	return cfg.Prefix + string(suffix), nil
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
		if i+2 < len(runes) && runes[i+1] == '-' && runes[i+2] >= runes[i] && runes[i+2] <= 0xff {
			// Range: a-z
			for value := int(runes[i]); value <= int(runes[i+2]); value++ {
				add(byte(value))
			}
			i += 3
			continue
		}
		if runes[i] == '\\' && i+1 < len(runes) {
			// Escaped character: \( \) \! etc.
			i++
			if runes[i] <= 0xff {
				add(byte(runes[i]))
			}
			i++
			continue
		}
		if runes[i] <= 0xff {
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

// encodePeerID percent-encodes the peer ID string using the pre-compiled exclusion
// regex when shouldUrlEncode is true, otherwise URL-query-escapes it.
// Using the pre-compiled exclusionRE avoids recompiling the pattern on every announce.
func encodePeerID(peerID string, gen peerIDGeneratorConfig, exclusionRE *regexp.Regexp, hexCase string) string {
	if gen.ShouldURLEncode {
		return encodeBytesRE([]byte(peerID), exclusionRE, hexCase)
	}
	return url.QueryEscape(peerID)
}

// compileExclusion compiles the exclusion regex once at load time.
// Returns nil on empty pattern or compile error.
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
