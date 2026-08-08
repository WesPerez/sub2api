package service

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const dynamicAccountTestMarkerAlphabet = "abcdefghjkmnpqrs"

type dynamicAccountTestTask struct {
	Prompt string
	Marker string
	Result string
}

func newDynamicAccountTestTask() (dynamicAccountTestTask, error) {
	return newDynamicAccountTestTaskWithEntropy(rand.Reader)
}

func newDynamicAccountTestTaskWithEntropy(entropy io.Reader) (dynamicAccountTestTask, error) {
	var raw [10]byte
	if _, err := io.ReadFull(entropy, raw[:]); err != nil {
		return dynamicAccountTestTask{}, fmt.Errorf("read test prompt entropy: %w", err)
	}

	left := 10 + int(binary.BigEndian.Uint16(raw[0:2])%90)
	right := 10 + int(binary.BigEndian.Uint16(raw[2:4])%90)
	var markerBytes [12]byte
	for index, value := range raw[4:] {
		markerBytes[index*2] = dynamicAccountTestMarkerAlphabet[value>>4]
		markerBytes[index*2+1] = dynamicAccountTestMarkerAlphabet[value&0x0f]
	}
	marker := string(markerBytes[:])
	result := strconv.Itoa(left + right)

	return dynamicAccountTestTask{
		Prompt: fmt.Sprintf(
			"Connection check %s: add %d and %d. Reply in one short sentence containing the marker %s and the computed sum.",
			marker, left, right, marker,
		),
		Marker: marker,
		Result: result,
	}, nil
}

func (task dynamicAccountTestTask) validate(response string) error {
	marker := strings.ToLower(strings.TrimSpace(task.Marker))
	result := strings.TrimSpace(task.Result)
	resultToken := false
	for _, token := range strings.FieldsFunc(response, func(r rune) bool { return r < '0' || r > '9' }) {
		if token == result {
			resultToken = true
			break
		}
	}
	if marker == "" || result == "" || !strings.Contains(strings.ToLower(response), marker) || !resultToken {
		return fmt.Errorf("dynamic connection task returned an unexpected response")
	}
	return nil
}
