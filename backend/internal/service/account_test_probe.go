package service

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

type accountTestProbeContextKey struct{}

// AccountTestProbe is a one-shot, meaningful text probe used by admin Test Connection.
// It covers light structured output, field extraction, and a small sort/compute step.
type AccountTestProbe struct {
	Family   string
	Prompt   string
	Expected map[string]any
}

// NewAccountTestProbe selects a small task family and natural random values.
func NewAccountTestProbe() (*AccountTestProbe, error) {
	return NewAccountTestProbeWithEntropy(rand.Reader)
}

// NewAccountTestProbeWithEntropy is the injectable constructor used by production and tests.
func NewAccountTestProbeWithEntropy(r io.Reader) (*AccountTestProbe, error) {
	if r == nil {
		r = rand.Reader
	}
	builders := []func(io.Reader) (*AccountTestProbe, error){
		buildInventoryProbe,
		buildTemperatureProbe,
		buildOrderProbe,
	}
	idx, err := readRandIntn(r, len(builders))
	if err != nil {
		return nil, fmt.Errorf("select probe family: %w", err)
	}
	return builders[idx](r)
}

func withAccountTestProbe(ctx context.Context, probe *AccountTestProbe) context.Context {
	if probe == nil {
		return ctx
	}
	return context.WithValue(ctx, accountTestProbeContextKey{}, probe)
}

func accountTestProbeFromContext(ctx context.Context) *AccountTestProbe {
	if ctx == nil {
		return nil
	}
	probe, _ := ctx.Value(accountTestProbeContextKey{}).(*AccountTestProbe)
	return probe
}

// ValidateResponse extracts a JSON object from model text and checks expected semantics strictly.
func (p *AccountTestProbe) ValidateResponse(text string) error {
	if p == nil {
		return fmt.Errorf("probe is nil")
	}
	obj, err := extractJSONObject(text)
	if err != nil {
		return fmt.Errorf("expected JSON object in model response: %w", err)
	}
	for key, want := range p.Expected {
		got, ok := obj[key]
		if !ok {
			return fmt.Errorf("missing field %q", key)
		}
		if err := assertProbeValueEqual(key, want, got); err != nil {
			return err
		}
	}
	return nil
}

func assertProbeValueEqual(key string, want, got any) error {
	switch w := want.(type) {
	case string:
		g, ok := got.(string)
		if !ok {
			return fmt.Errorf("field %q: expected string %q, got %v", key, w, got)
		}
		if g != w {
			return fmt.Errorf("field %q: expected %q, got %q", key, w, g)
		}
	case int:
		n, ok := asProbeInt(got)
		if !ok || n != w {
			return fmt.Errorf("field %q: expected %d, got %v", key, w, got)
		}
	case int64:
		n, ok := asProbeInt(got)
		if !ok || int64(n) != w {
			return fmt.Errorf("field %q: expected %d, got %v", key, w, got)
		}
	case float64:
		n, ok := asProbeFloat(got)
		if !ok || math.Abs(n-w) > 1e-9 {
			return fmt.Errorf("field %q: expected %v, got %v", key, w, got)
		}
	case []string:
		arr, ok := asProbeStringSlice(got)
		if !ok {
			return fmt.Errorf("field %q: expected string array %v, got %v", key, w, got)
		}
		if len(arr) != len(w) {
			return fmt.Errorf("field %q: expected %v, got %v", key, w, arr)
		}
		for i := range w {
			if arr[i] != w[i] {
				return fmt.Errorf("field %q: expected %v, got %v", key, w, arr)
			}
		}
	case []any:
		wantStr := make([]string, 0, len(w))
		for _, item := range w {
			wantStr = append(wantStr, fmt.Sprint(item))
		}
		return assertProbeValueEqual(key, wantStr, got)
	default:
		wb, _ := json.Marshal(want)
		gb, _ := json.Marshal(got)
		if string(wb) != string(gb) {
			return fmt.Errorf("field %q: expected %s, got %s", key, string(wb), string(gb))
		}
	}
	return nil
}

func asProbeInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		if math.Trunc(n) == n {
			return int(n), true
		}
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i), true
		}
		f, err := n.Float64()
		if err == nil && math.Trunc(f) == f {
			return int(f), true
		}
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err == nil {
			return i, true
		}
	}
	return 0, false
}

func asProbeFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

func asProbeStringSlice(v any) ([]string, bool) {
	switch arr := v.(type) {
	case []string:
		return arr, true
	case []any:
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		return nil, false
	}
}

// extractJSONObject tolerantly finds the first JSON object in model output.
// Supports optional markdown fences and leading/trailing prose.
func extractJSONObject(text string) (map[string]any, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("empty response")
	}
	fence := "```"
	if strings.HasPrefix(trimmed, fence) {
		trimmed = strings.TrimPrefix(trimmed, fence)
		trimmed = strings.TrimSpace(trimmed)
		if len(trimmed) >= 4 && strings.EqualFold(trimmed[:4], "json") {
			trimmed = strings.TrimSpace(trimmed[4:])
		}
		if idx := strings.LastIndex(trimmed, fence); idx >= 0 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}
	}

	start := strings.Index(trimmed, "{")
	if start < 0 {
		return nil, fmt.Errorf("no JSON object found")
	}
	end := findMatchingJSONBrace(trimmed, start)
	if end < 0 {
		return nil, fmt.Errorf("unterminated JSON object")
	}
	raw := trimmed[start : end+1]
	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func findMatchingJSONBrace(s string, start int) int {
	if start < 0 || start >= len(s) || s[start] != '{' {
		return -1
	}
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inString {
			if escape {
				escape = false
				continue
			}
			if ch == '\\' {
				escape = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func readRandIntn(r io.Reader, n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("invalid n=%d", n)
	}
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint64(buf[:]) % uint64(n)), nil
}

func pickProbeStrings(r io.Reader, pool []string, count int) ([]string, error) {
	if count <= 0 || count > len(pool) {
		return nil, fmt.Errorf("invalid pick count")
	}
	items := append([]string(nil), pool...)
	for i := 0; i < count; i++ {
		j, err := readRandIntn(r, len(items)-i)
		if err != nil {
			return nil, err
		}
		j += i
		items[i], items[j] = items[j], items[i]
	}
	return items[:count], nil
}

func readProbeCount(r io.Reader, min, max int) (int, error) {
	if max < min {
		return 0, fmt.Errorf("invalid range")
	}
	n, err := readRandIntn(r, max-min+1)
	if err != nil {
		return 0, err
	}
	return min + n, nil
}

var (
	probeFruitPool = []string{"apple", "banana", "cherry", "mango", "peach", "plum", "grape", "lemon"}
	probeCityPool  = []string{"Tokyo", "Paris", "Cairo", "Seoul", "Lima", "Oslo", "Perth", "Rome"}
	probeItemPool  = []string{"notebook", "pencil", "eraser", "ruler", "stapler", "folder", "marker", "binder"}
)

func buildInventoryProbe(r io.Reader) (*AccountTestProbe, error) {
	names, err := pickProbeStrings(r, probeFruitPool, 3)
	if err != nil {
		return nil, err
	}
	counts := make([]int, 3)
	for i := range counts {
		c, err := readProbeCount(r, 2, 9)
		if err != nil {
			return nil, err
		}
		counts[i] = c
	}
	maxIdx := 0
	for i := 1; i < len(counts); i++ {
		if counts[i] > counts[maxIdx] {
			maxIdx = i
		}
	}
	for i := range counts {
		if i != maxIdx && counts[i] == counts[maxIdx] {
			counts[i]--
			if counts[i] < 1 {
				counts[i] = 1
				counts[maxIdx]++
			}
		}
	}

	total := 0
	lines := make([]string, 0, len(names))
	for i, name := range names {
		total += counts[i]
		lines = append(lines, fmt.Sprintf("- %s x %d", name, counts[i]))
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	top := names[maxIdx]

	prompt := "Connection probe: answer with a single JSON object only. No markdown, no explanation.\n" +
		"Inventory lines:\n" + strings.Join(lines, "\n") + "\n" +
		"Requirements:\n" +
		"1) items: fruit names sorted A-Z\n" +
		"2) total: sum of all quantities\n" +
		"3) top: fruit with the largest quantity\n" +
		"Schema: {\"items\":[\"...\"],\"total\":0,\"top\":\"...\"}"

	return &AccountTestProbe{
		Family: "inventory",
		Prompt: prompt,
		Expected: map[string]any{
			"items": sorted,
			"total": total,
			"top":   top,
		},
	}, nil
}

func buildTemperatureProbe(r io.Reader) (*AccountTestProbe, error) {
	cities, err := pickProbeStrings(r, probeCityPool, 3)
	if err != nil {
		return nil, err
	}
	base, err := readProbeCount(r, 8, 24)
	if err != nil {
		return nil, err
	}
	firstGap, err := readProbeCount(r, 2, 4)
	if err != nil {
		return nil, err
	}
	secondGap, err := readProbeCount(r, 2, 4)
	if err != nil {
		return nil, err
	}
	temps := []int{base, base + firstGap, base + firstGap + secondGap}
	for i := 0; i < len(temps)-1; i++ {
		j, err := readRandIntn(r, len(temps)-i)
		if err != nil {
			return nil, err
		}
		j += i
		temps[i], temps[j] = temps[j], temps[i]
	}
	hotIdx, coldIdx := 0, 0
	for i := 1; i < len(temps); i++ {
		if temps[i] > temps[hotIdx] {
			hotIdx = i
		}
		if temps[i] < temps[coldIdx] {
			coldIdx = i
		}
	}
	sum := 0
	lines := make([]string, 0, len(cities))
	for i, city := range cities {
		sum += temps[i]
		lines = append(lines, fmt.Sprintf("- %s: %dC", city, temps[i]))
	}
	avg := int(math.Round(float64(sum) / float64(len(temps))))

	prompt := "Connection probe: answer with a single JSON object only. No markdown, no explanation.\n" +
		"City temperatures:\n" + strings.Join(lines, "\n") + "\n" +
		"Requirements:\n" +
		"1) hottest: city with the highest temperature\n" +
		"2) coldest: city with the lowest temperature\n" +
		"3) average: arithmetic mean of temperatures, rounded to nearest integer\n" +
		"Schema: {\"hottest\":\"...\",\"coldest\":\"...\",\"average\":0}"

	return &AccountTestProbe{
		Family: "temperature",
		Prompt: prompt,
		Expected: map[string]any{
			"hottest": cities[hotIdx],
			"coldest": cities[coldIdx],
			"average": avg,
		},
	}, nil
}

func buildOrderProbe(r io.Reader) (*AccountTestProbe, error) {
	names, err := pickProbeStrings(r, probeItemPool, 3)
	if err != nil {
		return nil, err
	}
	prices := make([]int, 3)
	for i := range prices {
		price, err := readProbeCount(r, 2, 15)
		if err != nil {
			return nil, err
		}
		prices[i] = price
	}
	sum := 0
	lines := make([]string, 0, len(names))
	for i, name := range names {
		sum += prices[i]
		lines = append(lines, fmt.Sprintf("- %s: %d", name, prices[i]))
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)

	prompt := "Connection probe: answer with a single JSON object only. No markdown, no explanation.\n" +
		"Order lines (name: price):\n" + strings.Join(lines, "\n") + "\n" +
		"Requirements:\n" +
		"1) names: item names sorted A-Z\n" +
		"2) sum: total of all prices\n" +
		"3) count: number of order lines\n" +
		"Schema: {\"names\":[\"...\"],\"sum\":0,\"count\":0}"

	return &AccountTestProbe{
		Family: "order",
		Prompt: prompt,
		Expected: map[string]any{
			"names": sorted,
			"sum":   sum,
			"count": len(names),
		},
	}, nil
}
