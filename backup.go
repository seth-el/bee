package bee

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const BackupFormatVersion = 1

type BackupManifest struct {
	FormatVersion    int               `json:"format_version"`
	CreatedAt        time.Time         `json:"created_at"`
	BeeCommit        string            `json:"bee_commit,omitempty"`
	OpsConfigVersion int               `json:"ops_config_version"`
	IncludedDomains  []string          `json:"included_domains"`
	Checksums        map[string]string `json:"checksums"`
	TotalBytes       int64             `json:"total_bytes"`
}

type BackupOptions struct {
	Destination      string
	IncludeArtifacts bool
	MaxBytes         int64
}

type RestoreOptions struct {
	Source   string
	Apply    bool
	MaxBytes int64
}

type RestoreConflict struct {
	Domain string `json:"domain"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type RestoreReport struct {
	Source        string            `json:"source"`
	FormatVersion int               `json:"format_version"`
	Conflicts     []RestoreConflict `json:"conflicts,omitempty"`
	WouldWrite    int               `json:"would_write"`
	WouldSkip     int               `json:"would_skip"`
	Applied       bool              `json:"applied"`
	Errors        []string          `json:"errors,omitempty"`
}

type backupEntry struct {
	archivePath string
	sourcePath  string
	domain      string
	size        int64
}

type restoreFile struct {
	archivePath string
	domain      string
	data        []byte
}

func Backup(ctx context.Context, cfg OpsConfig, options BackupOptions) (BackupManifest, error) {
	if ctx == nil {
		return BackupManifest{}, errors.New("backup context is nil")
	}
	if err := cfg.Validate(); err != nil {
		return BackupManifest{}, err
	}
	if strings.TrimSpace(options.Destination) == "" {
		return BackupManifest{}, errors.New("backup destination is required")
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = 1 << 30
	}
	if _, err := os.Lstat(options.Destination); err == nil {
		return BackupManifest{}, errors.New("backup destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return BackupManifest{}, errors.New("inspect backup destination")
	}

	entries, err := collectBackupEntries(ctx, cfg, options)
	if err != nil {
		return BackupManifest{}, err
	}
	manifest := BackupManifest{
		FormatVersion:    BackupFormatVersion,
		CreatedAt:        time.Now().UTC(),
		OpsConfigVersion: cfg.Version,
		Checksums:        make(map[string]string, len(entries)),
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return BackupManifest{}, err
		}
		checksum, err := fileChecksum(entry.sourcePath)
		if err != nil {
			return BackupManifest{}, err
		}
		manifest.Checksums[entry.archivePath] = checksum
		manifest.TotalBytes += entry.size
		manifest.IncludedDomains = append(manifest.IncludedDomains, entry.domain)
	}
	if manifest.TotalBytes > options.MaxBytes {
		return BackupManifest{}, errors.New("backup exceeds byte limit")
	}
	manifest.IncludedDomains = backupUniqueStrings(manifest.IncludedDomains)
	if err := writeBackup(ctx, options.Destination, manifest, entries); err != nil {
		return BackupManifest{}, err
	}
	return manifest, nil
}

func Restore(ctx context.Context, cfg OpsConfig, options RestoreOptions) (RestoreReport, error) {
	report := RestoreReport{Source: options.Source}
	if ctx == nil {
		return report, errors.New("restore context is nil")
	}
	if err := cfg.Validate(); err != nil {
		return report, err
	}
	if strings.TrimSpace(options.Source) == "" {
		return report, errors.New("restore source is required")
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = 1 << 30
	}
	manifest, files, err := readBackup(ctx, options.Source, options.MaxBytes)
	if err != nil {
		return report, err
	}
	report.FormatVersion = manifest.FormatVersion

	for _, file := range files {
		destination, err := restoreDestination(cfg, file.archivePath)
		if err != nil {
			return report, err
		}
		info, statErr := os.Lstat(destination)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return report, errors.New("restore destination is unsafe")
			}
			checksum, checksumErr := fileChecksum(destination)
			if checksumErr == nil && checksum == manifest.Checksums[file.archivePath] {
				report.WouldSkip++
				continue
			}
			report.Conflicts = append(report.Conflicts, RestoreConflict{
				Domain: file.domain,
				Path:   file.archivePath,
				Reason: "destination exists and differs",
			})
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return report, statErr
		}
		report.WouldWrite++
	}
	if len(report.Conflicts) > 0 || !options.Apply {
		return report, nil
	}
	for _, file := range files {
		destination, err := restoreDestination(cfg, file.archivePath)
		if err != nil {
			return report, err
		}
		if _, err := os.Stat(destination); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return report, err
		}
		if err := writeRestoreFile(ctx, destination, file.data); err != nil {
			return report, err
		}
	}
	report.Applied = true
	return report, nil
}

func collectBackupEntries(ctx context.Context, cfg OpsConfig, options BackupOptions) ([]backupEntry, error) {
	var entries []backupEntry
	addRoot := func(domain, root string, include bool) error {
		if !include {
			return nil
		}
		return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("backup refuses symlink entries")
			}
			if info.IsDir() {
				return nil
			}
			base := strings.ToLower(filepath.Base(path))
			if strings.HasSuffix(base, ".lock") || strings.HasSuffix(base, ".search.idx") || strings.Contains(base, "token") || strings.Contains(base, "secret") {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			entries = append(entries, backupEntry{
				archivePath: filepath.ToSlash(filepath.Join(domain, relative)),
				sourcePath:  path,
				domain:      domain,
				size:        info.Size(),
			})
			return nil
		})
	}
	if err := addRoot("sessions", cfg.SessionRoot, true); err != nil {
		return nil, err
	}
	if err := addRoot("memory", cfg.MemoryRoot, true); err != nil {
		return nil, err
	}
	if err := addRoot("diagnostics", cfg.DiagnosticsRoot, true); err != nil {
		return nil, err
	}
	if err := addRoot("artifacts", cfg.ArtifactRoot, options.IncludeArtifacts); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(cfg.JobStore); err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		entries = append(entries, backupEntry{archivePath: "jobs/" + filepath.Base(cfg.JobStore), sourcePath: cfg.JobStore, domain: "jobs", size: info.Size()})
	}
	return entries, nil
}

func writeBackup(ctx context.Context, destination string, manifest BackupManifest, entries []backupEntry) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".bee-backup-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	gzipWriter := gzip.NewWriter(temporary)
	tarWriter := tar.NewWriter(gzipWriter)
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(manifestData)), ModTime: manifest.CreatedAt}); err != nil {
		return err
	}
	if _, err := tarWriter.Write(manifestData); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := os.Open(entry.sourcePath)
		if err != nil {
			return err
		}
		if err := tarWriter.WriteHeader(&tar.Header{Name: entry.archivePath, Mode: 0600, Size: entry.size}); err != nil {
			file.Close()
			return err
		}
		_, copyErr := io.Copy(tarWriter, file)
		file.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, destination)
}

func readBackup(ctx context.Context, path string, maxBytes int64) (BackupManifest, []restoreFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return BackupManifest{}, nil, err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return BackupManifest{}, nil, errors.New("invalid backup compression")
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	var manifest BackupManifest
	var files []restoreFile
	var total int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return manifest, nil, errors.New("invalid backup archive")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return manifest, nil, errors.New("backup contains non-regular entry")
		}
		if header.Name == "manifest.json" {
			data, err := io.ReadAll(io.LimitReader(tarReader, 1<<20))
			if err != nil {
				return manifest, nil, err
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return manifest, nil, errors.New("invalid backup manifest")
			}
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if header.Name == "" || filepath.IsAbs(header.Name) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || strings.Contains(header.Name, "\\") {
			return manifest, nil, errors.New("backup contains unsafe path")
		}
		if header.Size < 0 || header.Size > maxBytes || total+header.Size > maxBytes {
			return manifest, nil, errors.New("backup exceeds byte limit")
		}
		data, err := io.ReadAll(io.LimitReader(tarReader, header.Size+1))
		if err != nil || int64(len(data)) != header.Size {
			return manifest, nil, errors.New("backup entry size mismatch")
		}
		parts := strings.SplitN(filepath.ToSlash(clean), "/", 2)
		if len(parts) != 2 {
			return manifest, nil, errors.New("backup path is invalid")
		}
		switch parts[0] {
		case "sessions", "memory", "diagnostics", "artifacts", "jobs":
		default:
			return manifest, nil, errors.New("backup domain is not allowed")
		}
		files = append(files, restoreFile{archivePath: filepath.ToSlash(clean), domain: parts[0], data: data})
		total += header.Size
		if err := ctx.Err(); err != nil {
			return manifest, nil, err
		}
	}
	if manifest.FormatVersion != BackupFormatVersion {
		return manifest, nil, errors.New("unsupported backup format")
	}
	for _, file := range files {
		sum := sha256.Sum256(file.data)
		if hex.EncodeToString(sum[:]) != manifest.Checksums[file.archivePath] {
			return manifest, nil, errors.New("backup checksum mismatch")
		}
	}
	return manifest, files, nil
}

func restoreDestination(cfg OpsConfig, archive string) (string, error) {
	parts := strings.SplitN(filepath.ToSlash(archive), "/", 2)
	if len(parts) != 2 {
		return "", errors.New("backup path is invalid")
	}
	var root string
	switch parts[0] {
	case "sessions":
		root = cfg.SessionRoot
	case "memory":
		root = cfg.MemoryRoot
	case "diagnostics":
		root = cfg.DiagnosticsRoot
	case "artifacts":
		root = cfg.ArtifactRoot
	case "jobs":
		root = filepath.Dir(cfg.JobStore)
	default:
		return "", errors.New("backup domain is not allowed")
	}
	destination := filepath.Clean(filepath.Join(root, filepath.FromSlash(parts[1])))
	if !withinCleanupRoot(root, destination) {
		return "", errors.New("restore path escaped root")
	}
	return destination, nil
}

func writeRestoreFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".restore-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func fileChecksum(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
func backupUniqueStrings(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
