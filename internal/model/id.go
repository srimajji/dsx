package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	projectIDLength    = 20
	maxWorkspaceLength = 24
)

type ProjectID string

type WorkspaceName string

type RunID string

func NewProjectID(canonicalRoot string) (ProjectID, error) {
	if canonicalRoot == "" || !filepath.IsAbs(canonicalRoot) || filepath.Clean(canonicalRoot) != canonicalRoot {
		return "", errors.New("canonical project root must be a clean absolute path")
	}
	digest := sha256.Sum256([]byte(canonicalRoot))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])
	return ProjectID(strings.ToLower(encoded[:projectIDLength])), nil
}

func ParseProjectID(value string) (ProjectID, error) {
	if len(value) != projectIDLength || !isLowerBase32(value) {
		return "", fmt.Errorf("invalid project ID %q", value)
	}
	return ProjectID(value), nil
}

func ParseWorkspaceName(value string) (WorkspaceName, error) {
	if len(value) == 0 || len(value) > maxWorkspaceLength || value[0] == '-' || value[len(value)-1] == '-' {
		return "", fmt.Errorf("invalid workspace name %q", value)
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return "", fmt.Errorf("invalid workspace name %q", value)
		}
	}
	return WorkspaceName(value), nil
}

func NewRunID(now time.Time) (RunID, error) {
	var bytes [16]byte
	millis := now.UTC().UnixMilli()
	if millis < 0 || millis > 1<<48-1 {
		return "", errors.New("time is outside UUIDv7 range")
	}
	for index := 5; index >= 0; index-- {
		bytes[index] = byte(millis)
		millis >>= 8
	}
	if _, err := rand.Read(bytes[6:]); err != nil {
		return "", fmt.Errorf("generate run ID: %w", err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x70
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return formatUUID(bytes), nil
}

func ParseRunID(value string) (RunID, error) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return "", fmt.Errorf("invalid run ID %q", value)
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(compact)
	if err != nil || len(decoded) != 16 || decoded[6]>>4 != 7 || decoded[8]>>6 != 2 {
		return "", fmt.Errorf("invalid UUIDv7 run ID %q", value)
	}
	return RunID(strings.ToLower(value)), nil
}

func formatUUID(value [16]byte) RunID {
	encoded := hex.EncodeToString(value[:])
	return RunID(encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32])
}

func isLowerBase32(value string) bool {
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			return false
		}
	}
	return true
}
