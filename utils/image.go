package utils

import (
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/disintegration/imaging"
	"github.com/google/uuid"
)

const (
	// MaxImageSize 大于此大小（字节）自动压缩
	MaxImageSize = 2 * 1024 * 1024 // 2MB
	// CompressQuality WebP 压缩质量
	CompressQuality = 80
	// MaxWidth 压缩后最大宽度
	MaxWidth = 1600
)

// ProcessUploadedFile 处理上传的文件（仅允许图片和视频）
func ProcessUploadedFile(srcPath, uploadsDir string) (string, int64, error) {
	stat, err := os.Stat(srcPath)
	if err != nil {
		return "", 0, fmt.Errorf("stat file: %w", err)
	}

	// 校验文件类型
	if err := validateFileType(srcPath); err != nil {
		os.Remove(srcPath)
		return "", 0, err
	}

	ext := strings.ToLower(filepath.Ext(srcPath))
	uniqueID := uuid.New().String()[:8]
	baseName := sanitizeFilename(strings.TrimSuffix(filepath.Base(srcPath), ext))

	// 小图：直接重命名，加 UUID 前缀避免冲突
	if stat.Size() <= MaxImageSize && (ext == ".jpg" || ext == ".jpeg" || ext == ".png") {
		destFilename := uniqueID + "_" + baseName + ext
		destPath := filepath.Join(uploadsDir, destFilename)
		if err := os.Rename(srcPath, destPath); err != nil {
			return "", 0, fmt.Errorf("move file: %w", err)
		}
		return destFilename, stat.Size(), nil
	}

	// 尝试解码图片获取尺寸
	imgConfig, imgErr := func() (image.Config, error) {
		f, err := os.Open(srcPath)
		if err != nil {
			return image.Config{}, err
		}
		defer f.Close()
		cfg, _, err := image.DecodeConfig(f)
		return cfg, err
	}()

	if imgErr != nil {
		// 不是图片文件，直接保存
		destFilename := uniqueID + "_" + baseName + ext
		destPath := filepath.Join(uploadsDir, destFilename)
		if err := os.Rename(srcPath, destPath); err != nil {
			return "", 0, fmt.Errorf("move non-image file: %w", err)
		}
		return destFilename, stat.Size(), nil
	}

	// 只有大图才需要压缩
	if stat.Size() <= MaxImageSize {
		destFilename := uniqueID + "_" + baseName + ext
		destPath := filepath.Join(uploadsDir, destFilename)
		if err := os.Rename(srcPath, destPath); err != nil {
			return "", 0, fmt.Errorf("move file: %w", err)
		}
		return destFilename, stat.Size(), nil
	}

	// 完整解码大图用于压缩
	srcImg, formatName, err := func() (image.Image, string, error) {
		f, err := os.Open(srcPath)
		if err != nil {
			return nil, "", err
		}
		defer f.Close()
		img, fmt, err := image.Decode(f)
		return img, fmt, err
	}()
	if err != nil {
		return "", 0, fmt.Errorf("decode image: %w", err)
	}

	// 如果不是需要重新编码的格式，也直接保存
	// 注意：Go 标准库 image.Decode 不会返回 "webp"，此条件仅 jpeg/png 会走到重新编码
	if formatName != "jpeg" && formatName != "png" {
		destFilename := uniqueID + "_" + baseName + ext
		destPath := filepath.Join(uploadsDir, destFilename)
		if err := os.Rename(srcPath, destPath); err != nil {
			return "", 0, fmt.Errorf("move file: %w", err)
		}
		return destFilename, stat.Size(), nil
	}

	// 等比例缩放
	resized := srcImg
	if imgConfig.Width > MaxWidth {
		resized = imaging.Resize(srcImg, MaxWidth, 0, imaging.Lanczos)
	}

	// 保存为压缩后的 JPEG
	destFilename := uniqueID + "_" + baseName + ".jpg"
	destPath := filepath.Join(uploadsDir, destFilename)

	if err := imaging.Save(resized, destPath, imaging.JPEGQuality(CompressQuality)); err != nil {
		return "", 0, fmt.Errorf("save image: %w", err)
	}

	// 删除原始文件
	if err := os.Remove(srcPath); err != nil {
		log.Printf("[Image] failed to remove original file %s: %v", srcPath, err)
	}

	// 获取压缩后大小
	newStat, _ := os.Stat(destPath)
	newSize := int64(0)
	if newStat != nil {
		newSize = newStat.Size()
	}

	return destFilename, newSize, nil
}

// SaveUploadedFile 保存上传文件到 uploads 目录（不做处理）
func SaveUploadedFile(srcPath, uploadsDir string) (string, error) {
	if err := validateFileType(srcPath); err != nil {
		os.Remove(srcPath)
		return "", err
	}
	uniqueID := uuid.New().String()[:8]
	ext := strings.ToLower(filepath.Ext(filepath.Base(srcPath)))
	baseName := sanitizeFilename(strings.TrimSuffix(filepath.Base(srcPath), ext))
	destFilename := uniqueID + "_" + baseName + ext
	destPath := filepath.Join(uploadsDir, destFilename)
	if err := os.Rename(srcPath, destPath); err != nil {
		// 如果 Rename 失败（跨盘），使用 Copy
		src, err := os.Open(srcPath)
		if err != nil {
			return "", err
		}
		defer src.Close()

		dst, err := os.Create(destPath)
		if err != nil {
			return "", err
		}
		defer dst.Close()

		if _, err := io.Copy(dst, src); err != nil {
			return "", err
		}
		os.Remove(srcPath)
	}

	return destFilename, nil
}

// sanitizeFilename 净化文件名，去除逗号、&、=、?、# 等 URL 特殊字符和中文/特殊 Unicode 字符
// 保留字母、数字、下划线、连字符、点号
var filenameSanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9_\-.]`)

func sanitizeFilename(name string) string {
	// 替换 URL 特殊字符为下划线
	cleaned := filenameSanitizeRe.ReplaceAllString(name, "_")
	// 合并连续下划线
	for strings.Contains(cleaned, "__") {
		cleaned = strings.ReplaceAll(cleaned, "__", "_")
	}
	// 去掉首尾下划线
	cleaned = strings.Trim(cleaned, "_")
	// 如果净化后为空，用 "file" 代替
	if cleaned == "" {
		cleaned = "file"
	}
	return cleaned
}

// allowedMimeTypes 允许上传的文件 MIME 类型
var allowedMimeTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true,
	"image/webp": true,
	"video/mp4":  true, "video/webm": true,
}

// validateFileType 读取文件头检测 MIME 类型并校验
func validateFileType(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open file for validation: %w", err)
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	contentType := http.DetectContentType(buf[:n])
	if !allowedMimeTypes[contentType] {
		return fmt.Errorf("不支持的文件类型: %s", contentType)
	}
	return nil
}
