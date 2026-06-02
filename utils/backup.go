package utils

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BackupConfig 备份配置
type BackupConfig struct {
	ProjectRoot string // 项目根目录
	BackupDir   string // 备份存放目录
	DBPath      string // 数据库文件路径
	UploadsDir  string // uploads 目录路径
}

// DefaultBackupConfig 返回默认备份配置
func DefaultBackupConfig(projectRoot string) *BackupConfig {
	return &BackupConfig{
		ProjectRoot: projectRoot,
		BackupDir:   filepath.Join(projectRoot, "backups"),
		DBPath:      filepath.Join(projectRoot, "uniflow.db"),
		UploadsDir:  filepath.Join(projectRoot, "uploads"),
	}
}

// CreateBackup 创建全站备份
// 将 uniflow.db 和 uploads/ 打包为 .tar.gz 存放在 backups/ 目录
func CreateBackup(cfg *BackupConfig) (string, error) {
	// 确保备份目录存在
	if err := os.MkdirAll(cfg.BackupDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create backup directory: %w", err)
	}

	// 生成备份文件名: uniflow_backup_2006-01-02_150405.tar.gz
	timestamp := time.Now().Format("2006-01-02_150405")
	backupFile := filepath.Join(cfg.BackupDir, fmt.Sprintf("uniflow_backup_%s.tar.gz", timestamp))

	// 创建 gzip 压缩文件
	f, err := os.Create(backupFile)
	if err != nil {
		return "", fmt.Errorf("failed to create backup file: %w", err)
	}
	defer f.Close()

	gzw := gzip.NewWriter(f)
	defer gzw.Close()

	tw := tar.NewWriter(gzw)
	defer tw.Close()

	// 收集需要备份的文件和目录
	backupItems := []string{
		cfg.DBPath,     // 数据库文件
		cfg.UploadsDir, // uploads 目录
	}

	// 同时备份数库 WAL 和 SHM 文件（SQLite WAL 模式的附加文件）
	for _, ext := range []string{"-wal", "-shm"} {
		walFile := cfg.DBPath + ext
		if _, err := os.Stat(walFile); err == nil {
			backupItems = append(backupItems, walFile)
		}
	}

	addedFiles := 0

	for _, item := range backupItems {
		info, err := os.Stat(item)
		if err != nil {
			if os.IsNotExist(err) {
				continue // 文件不存在则跳过
			}
			return "", fmt.Errorf("failed to stat %s: %w", item, err)
		}

		if !info.Mode().IsRegular() && !info.IsDir() {
			continue // 跳过非常规文件
		}

		// 计算在 tar 中的相对路径
		relPath, err := filepath.Rel(cfg.ProjectRoot, item)
		if err != nil {
			return "", fmt.Errorf("failed to get relative path: %w", err)
		}

		if info.IsDir() {
			// 递归添加目录内容
			count, err := addDirToTar(tw, item, relPath)
			if err != nil {
				return "", fmt.Errorf("failed to add directory %s: %w", item, err)
			}
			addedFiles += count
		} else {
			// 添加单个文件
			if err := addFileToTar(tw, item, relPath, info); err != nil {
				return "", fmt.Errorf("failed to add file %s: %w", item, err)
			}
			addedFiles++
		}
	}

	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("failed to finalize tar archive: %w", err)
	}

	// 获取备份文件大小（可用于日志或返回）
	fileInfo, _ := os.Stat(backupFile)
	if fileInfo != nil {
		log.Printf("[Backup] created: %s (%s)", backupFile, FormatFileSize(fileInfo.Size()))
	}

	return backupFile, nil
}

// RestoreBackup 从备份文件恢复
// 解压并覆盖当前的 uniflow.db 和 uploads/ 目录
func RestoreBackup(cfg *BackupConfig, backupFilePath string) error {
	// 验证备份文件存在
	if _, err := os.Stat(backupFilePath); err != nil {
		return fmt.Errorf("backup file not found: %s", backupFilePath)
	}

	// 验证是 .tar.gz 文件
	if !strings.HasSuffix(backupFilePath, ".tar.gz") {
		return fmt.Errorf("invalid backup file: must be .tar.gz format")
	}

	// 打开并解压 gzip
	f, err := os.Open(backupFilePath)
	if err != nil {
		return fmt.Errorf("failed to open backup file: %w", err)
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	restoredFiles := 0

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break // 读取完毕
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		// 安全检查：防止路径穿越
		targetPath := filepath.Join(cfg.ProjectRoot, header.Name)
		if !strings.HasPrefix(filepath.Clean(targetPath), filepath.Clean(cfg.ProjectRoot)+string(os.PathSeparator)) {
			return fmt.Errorf("security: path traversal detected in backup: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			// 创建目录
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", targetPath, err)
			}

		case tar.TypeReg:
			// 确保父目录存在
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return fmt.Errorf("failed to create parent dir: %w", err)
			}

			// 创建目标文件
			outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("failed to create file %s: %w", targetPath, err)
			}

			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return fmt.Errorf("failed to write file %s: %w", targetPath, err)
			}
			outFile.Close()
			restoredFiles++
		}
	}

	return nil
}

// ListBackups 列出所有备份文件
func ListBackups(backupDir string) ([]string, error) {
	var backups []string

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return backups, nil // 目录不存在返回空
		}
		return nil, fmt.Errorf("failed to read backup directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tar.gz") {
			backups = append(backups, filepath.Join(backupDir, entry.Name()))
		}
	}

	return backups, nil
}

// DeleteBackup 删除指定备份文件
func DeleteBackup(backupFilePath string) error {
	if err := os.Remove(backupFilePath); err != nil {
		return fmt.Errorf("failed to delete backup file: %w", err)
	}
	return nil
}

// addFileToTar 将单个文件添加到 tar 归档中
func addFileToTar(tw *tar.Writer, filePath, relPath string, info os.FileInfo) error {
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = filepath.ToSlash(relPath)

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(tw, f)
	return err
}

// addDirToTar 递归将目录内容添加到 tar 归档中
func addDirToTar(tw *tar.Writer, dirPath, relPath string) (int, error) {
	count := 0

	// 写入目录头
	info, err := os.Stat(dirPath)
	if err != nil {
		return 0, err
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return 0, err
	}
	header.Name = filepath.ToSlash(relPath) + "/"
	if err := tw.WriteHeader(header); err != nil {
		return 0, err
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return 0, err
	}

	for _, entry := range entries {
		fullPath := filepath.Join(dirPath, entry.Name())
		entryRelPath := filepath.Join(relPath, entry.Name())
		entryInfo, err := entry.Info()
		if err != nil {
			continue
		}

		if entry.IsDir() {
			subCount, err := addDirToTar(tw, fullPath, entryRelPath)
			if err != nil {
				return count, err
			}
			count += subCount
		} else {
			if err := addFileToTar(tw, fullPath, entryRelPath, entryInfo); err != nil {
				return count, err
			}
			count++
		}
	}

	return count, nil
}

// FormatFileSize 格式化文件大小
func FormatFileSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
