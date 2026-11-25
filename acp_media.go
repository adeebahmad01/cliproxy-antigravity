package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// agy acp ignores audio blocks and embedded resource blobs, but attaches a file given as a
// resource_link, so audio, video and documents are written to the workdir and linked.

const (
	acpMediaMaxBytes       = 100 << 20
	acpMediaDir            = ".cliproxy_media"
	acpMediaCacheTTL       = 24 * time.Hour
	acpMediaCacheMaxBytes  = 256 << 20
	acpMediaCacheMaxFiles  = 256
	acpMediaTempEvictAfter = 10 * time.Minute
	pcm16SampleRate        = 16000
)

var (
	mediaCacheMu   sync.Mutex
	activeMediaRef = map[string]int{}
)

func acquireMediaRefLocked(path string) {
	activeMediaRef[path]++
}

func releaseOneMediaRefLocked(path string) {
	if activeMediaRef[path] <= 1 {
		delete(activeMediaRef, path)
	} else {
		activeMediaRef[path]--
	}
}

func releaseMediaRefs(paths []string) {
	if len(paths) == 0 {
		return
	}
	mediaCacheMu.Lock()
	for _, path := range paths {
		releaseOneMediaRefLocked(path)
	}
	mediaCacheMu.Unlock()
}

func releaseAndClearAttachments(attachments *[]acpPromptBlock) {
	if attachments != nil && len(*attachments) > 0 {
		releaseMediaRefs(mediaPathsFromBlocks(*attachments))
		*attachments = nil
	}
}

func mediaPathsFromBlocks(blocks []acpPromptBlock) []string {
	var paths []string
	for _, b := range blocks {
		if b["type"] != "resource_link" {
			continue
		}
		uri, _ := b["uri"].(string)
		if p, ok := strings.CutPrefix(uri, "file://"); ok && p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

func ensurePrivateMediaDir(workdir string) (string, error) {
	base := workdir
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, acpMediaDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating media directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("inspecting media directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("media directory %s must be a real directory, not a symlink", dir)
	}
	if workdir == "" || runtime.GOOS != "windows" {
		if err := validateStagingDirPrivacy(dir, info); err != nil {
			return "", fmt.Errorf("media directory privacy check failed: %w", err)
		}
	}
	return dir, nil
}

func acpMediaBlock(block map[string]any, workdir string, requestKeep map[string]bool) (acpPromptBlock, string, error) {
	kind, _ := block["type"].(string)
	var data []byte
	var mimeType, name string
	var err error
	switch kind {
	case "input_audio":
		audio, _ := block["input_audio"].(map[string]any)
		encoded, _ := audio["data"].(string)
		format, _ := audio["format"].(string)
		if data, err = base64.StdEncoding.DecodeString(strings.TrimSpace(encoded)); err != nil {
			return nil, "", fmt.Errorf("invalid base64 audio: %w", err)
		}
		mimeType = sniffMediaType(data, "audio/"+format)
		if strings.EqualFold(format, "pcm16") && mimeType == "audio/pcm16" {
			data, mimeType = wavFromPCM16(data), "audio/wav"
		}
	case "video_url", "input_video":
		ref := nestedString(block, "video_url", "url")
		if ref == "" {
			ref, _ = block["url"].(string)
		}
		if ref == "" {
			ref, _ = block["video_url"].(string)
		}
		if data, mimeType, err = decodeMediaRef(ref, "video/mp4"); err != nil {
			return nil, "", err
		}
	case "file", "input_file":
		ref := nestedString(block, "file", "file_data")
		if ref == "" {
			ref, _ = block["file_data"].(string)
		}
		if ref == "" {
			ref = nestedString(block, "file", "file_url")
		}
		name = nestedString(block, "file", "filename")
		if data, mimeType, err = decodeMediaRef(ref, mimeFromName(name)); err != nil {
			return nil, "", err
		}
	default:
		return nil, "", fmt.Errorf("unsupported media block %q", kind)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("empty %s attachment", kind)
	}
	if len(data) > acpMediaMaxBytes {
		return nil, "", fmt.Errorf("attachment larger than %d bytes", acpMediaMaxBytes)
	}
	path, err := stageMedia(data, mimeType, name, workdir, requestKeep)
	if err != nil {
		return nil, "", err
	}
	if requestKeep != nil {
		requestKeep[path] = true
	}
	link := acpPromptBlock{"type": "resource_link", "uri": "file://" + path, "name": filepath.Base(path), "mimeType": mimeType}
	return link, mediaLabel(mimeType), nil
}

func nestedString(block map[string]any, outer, inner string) string {
	m, _ := block[outer].(map[string]any)
	s, _ := m[inner].(string)
	return strings.TrimSpace(s)
}

func decodeMediaRef(ref, fallbackType string) ([]byte, string, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return nil, "", fmt.Errorf("attachment has no data")
	case strings.HasPrefix(ref, "data:"):
		comma := strings.IndexByte(ref, ',')
		if comma < 0 || !strings.Contains(ref[:comma], ";base64") {
			return nil, "", fmt.Errorf("unsupported data URL: base64 encoding is required")
		}
		declared := strings.TrimPrefix(strings.SplitN(ref[:comma], ";", 2)[0], "data:")
		data, err := base64.StdEncoding.DecodeString(ref[comma+1:])
		if err != nil {
			return nil, "", fmt.Errorf("invalid base64 attachment: %w", err)
		}
		return data, sniffMediaType(data, nonEmpty(declared, fallbackType)), nil
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		data, mimeType, err := fetchRemoteAttachment(ref, acpMediaMaxBytes)
		if err != nil {
			return nil, "", err
		}
		return data, sniffMediaType(data, nonEmpty(mimeType, fallbackType)), nil
	default:
		data, err := base64.StdEncoding.DecodeString(ref)
		if err != nil {
			return nil, "", fmt.Errorf("attachment is not a data URL, an https URL or base64")
		}
		return data, sniffMediaType(data, fallbackType), nil
	}
}

// sniffMediaType trusts the bytes over the declared type: CLIProxyAPI labels every audio type it does
// not know (m4a, aac, webm) as mp3.
func sniffMediaType(data []byte, declared string) string {
	declared = strings.ToLower(strings.TrimSpace(declared))
	audio := strings.HasPrefix(declared, "audio/")
	switch {
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return "audio/wav"
	case len(data) >= 2 && data[0] == 0xFF && data[1]&0xF6 == 0xF0:
		return "audio/aac"
	case bytes.HasPrefix(data, []byte("ID3")), len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0:
		return "audio/mpeg"
	case bytes.HasPrefix(data, []byte("fLaC")):
		return "audio/flac"
	case bytes.HasPrefix(data, []byte("OggS")):
		return "audio/ogg"
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		if audio || string(data[8:11]) == "M4A" {
			return "audio/mp4"
		}
		return "video/mp4"
	case bytes.HasPrefix(data, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		if audio {
			return "audio/webm"
		}
		return "video/webm"
	case bytes.HasPrefix(data, []byte("%PDF")):
		return "application/pdf"
	}
	if declared != "" && declared != "application/octet-stream" {
		return declared
	}
	return http.DetectContentType(data)
}

func wavFromPCM16(pcm []byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(pcm16SampleRate), uint32(pcm16SampleRate * 2), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

var mediaExtensions = map[string]string{
	"audio/wav": ".wav", "audio/mpeg": ".mp3", "audio/flac": ".flac", "audio/ogg": ".ogg",
	"audio/mp4": ".m4a", "audio/webm": ".weba", "audio/aac": ".aac", "video/mp4": ".mp4",
	"video/webm": ".webm", "video/quicktime": ".mov", "application/pdf": ".pdf", "text/plain": ".txt",
	"text/csv": ".csv", "application/json": ".json", "text/markdown": ".md", "text/html": ".html",
}

func mimeFromName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	for t, e := range mediaExtensions {
		if e == ext {
			return t
		}
	}
	return mime.TypeByExtension(ext)
}

func mediaExtension(mimeType, name string) string {
	if ext := mediaExtensions[strings.SplitN(mimeType, ";", 2)[0]]; ext != "" {
		return ext
	}
	if ext := filepath.Ext(name); ext != "" && len(ext) <= 6 {
		return strings.ToLower(ext)
	}
	if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

func mediaLabel(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	default:
		return "file"
	}
}

func stageMedia(data []byte, mimeType, name, workdir string, requestKeep map[string]bool) (string, error) {
	mediaCacheMu.Lock()
	defer mediaCacheMu.Unlock()

	dir, err := ensurePrivateMediaDir(workdir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	path, err := filepath.Abs(filepath.Join(dir, hex.EncodeToString(sum[:12])+mediaExtension(mimeType, name)))
	if err != nil {
		return "", err
	}

	now := time.Now()
	if _, err := os.Stat(path); err == nil {
		_ = os.Chtimes(path, now, now)
		acquireMediaRefLocked(path)
		if err := evictMediaCacheLocked(dir, path, requestKeep, now); err != nil {
			releaseOneMediaRefLocked(path)
			return "", err
		}
		return path, nil
	}

	tmp, err := os.CreateTemp(dir, "media-*.tmp")
	if err != nil {
		return "", fmt.Errorf("writing attachment: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("writing attachment: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("writing attachment: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("writing attachment: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("writing attachment: %w", err)
	}
	acquireMediaRefLocked(path)
	if err := evictMediaCacheLocked(dir, path, requestKeep, now); err != nil {
		releaseOneMediaRefLocked(path)
		if !requestKeep[path] && activeMediaRef[path] == 0 {
			_ = os.Remove(path)
		}
		return "", err
	}
	return path, nil
}

type cachedMediaFile struct {
	path    string
	size    int64
	modTime time.Time
}

func isStagingTempFile(name string) bool {
	return strings.HasPrefix(name, "media-") && strings.HasSuffix(name, ".tmp")
}

func evictMediaCacheLocked(dir, keepPath string, requestKeep map[string]bool, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	isProtected := func(p string) bool {
		return p == keepPath || requestKeep[p] || activeMediaRef[p] > 0
	}
	var evictable []cachedMediaFile
	var totalBytes int64
	var protectedBytes int64
	var totalFiles int
	var protectedFiles int
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		p, err := filepath.Abs(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if isStagingTempFile(entry.Name()) {
			if now.Sub(info.ModTime()) > acpMediaTempEvictAfter {
				_ = os.Remove(p)
			}
			continue
		}
		if !isProtected(p) && now.Sub(info.ModTime()) > acpMediaCacheTTL {
			_ = os.Remove(p)
			continue
		}
		totalBytes += info.Size()
		totalFiles++
		if isProtected(p) {
			protectedBytes += info.Size()
			protectedFiles++
			continue
		}
		evictable = append(evictable, cachedMediaFile{path: p, size: info.Size(), modTime: info.ModTime()})
	}

	if totalFiles > acpMediaCacheMaxFiles || totalBytes > acpMediaCacheMaxBytes {
		sort.Slice(evictable, func(i, j int) bool {
			return evictable[i].modTime.Before(evictable[j].modTime)
		})
		for _, item := range evictable {
			if totalFiles <= acpMediaCacheMaxFiles && totalBytes <= acpMediaCacheMaxBytes {
				break
			}
			if os.Remove(item.path) == nil {
				totalFiles--
				totalBytes -= item.size
			}
		}
	}
	if protectedFiles > acpMediaCacheMaxFiles || protectedBytes > acpMediaCacheMaxBytes {
		return pError(errorCodeRequestScoped, fmt.Sprintf("in-flight attachments (%d files, %d bytes) exceed the media cache limit (%d files, %d bytes)", protectedFiles, protectedBytes, acpMediaCacheMaxFiles, acpMediaCacheMaxBytes), http.StatusRequestEntityTooLarge)
	}
	return nil
}
