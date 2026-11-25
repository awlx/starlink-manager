package starlink

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// gRPC-Web framing (shared by the device gateway):
//
//	frame := 1-byte flags | 4-byte big-endian length | payload
//
// Flag bit 0x80 marks a trailers frame; a trailers payload carries HTTP/2
// style trailer lines such as "grpc-status: 0". Flag 0 marks a data frame.

const (
	frameFlagData     byte = 0x00
	frameFlagTrailers byte = 0x80
)

// grpcStatusError carries the grpc-status code and message from the gateway.
type grpcStatusError struct {
	code    int32
	message string
}

func (e *grpcStatusError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("gateway grpc-status %d", e.code)
	}
	return fmt.Sprintf("gateway grpc-status %d: %s", e.code, e.message)
}

// encodeGRPCWeb wraps a protobuf message into a single gRPC-Web data frame.
func encodeGRPCWeb(msg []byte) []byte {
	out := make([]byte, 5+len(msg))
	out[0] = frameFlagData
	binary.BigEndian.PutUint32(out[1:5], uint32(len(msg)))
	copy(out[5:], msg)
	return out
}

// decodeGRPCWeb splits a gRPC-Web response into data frames and trailer
// frames. It returns the concatenated data payloads and the parsed trailer
// lines.
func decodeGRPCWeb(body []byte) (data [][]byte, trailers map[string]string, err error) {
	r := bytes.NewReader(body)
	trailers = map[string]string{}
	for {
		var head [5]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return data, trailers, nil
			}
			return nil, nil, fmt.Errorf("truncated grpc-web frame header: %w", err)
		}
		flags := head[0]
		length := binary.BigEndian.Uint32(head[1:5])
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, nil, fmt.Errorf("truncated grpc-web frame body (%d bytes): %w", length, err)
		}
		if flags&frameFlagTrailers != 0 {
			mergeTrailers(trailers, string(payload))
			continue
		}
		if len(payload) > 0 {
			data = append(data, payload)
		}
	}
}

func mergeTrailers(dst map[string]string, raw string) {
	for _, line := range bytes.Split([]byte(raw), []byte("\r\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		key, val, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		dst[strings.TrimSpace(string(key))] = strings.TrimSpace(string(val))
	}
}

// parseGRPCStatus extracts the numeric grpc-status trailer value; 0 means
// success. A missing trailer is treated as an error, mirroring strict
// gRPC-Web clients.
func parseGRPCStatus(trailers map[string]string) (int32, string, error) {
	raw, ok := trailers["grpc-status"]
	if !ok {
		return 0, "", errors.New("grpc-web response missing grpc-status trailer")
	}
	code, err := parseSmallInt(raw)
	if err != nil {
		return 0, "", fmt.Errorf("invalid grpc-status %q: %w", raw, err)
	}
	return code, trailers["grpc-message"], nil
}

func parseSmallInt(raw string) (int32, error) {
	var v int64
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", raw)
		}
		v = v*10 + int64(c-'0')
		if v > 1<<31 {
			return 0, fmt.Errorf("grpc-status too large: %q", raw)
		}
	}
	return int32(v), nil
}
