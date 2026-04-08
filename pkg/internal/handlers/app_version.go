package handlers

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
)

// AppVersionHandler 应用版本相关的处理器
type AppVersionHandler struct {
	appVersionSvc *services.AppVersionService
	uploadDir     string // 本地文件存储目录，如 "uploads/versions"
	baseURL       string // 公开访问的 Base URL，如 "https://release.shepaw.com"
}

// NewAppVersionHandler 创建应用版本处理器实例
// uploadDir: 本地上传目录（相对或绝对路径），留空则默认 "uploads/versions"
// baseURL:   文件公开访问的根 URL，留空则默认为空字符串（返回相对路径）
func NewAppVersionHandler(appVersionSvc *services.AppVersionService, uploadDir, baseURL string) *AppVersionHandler {
	if uploadDir == "" {
		uploadDir = "uploads/versions"
	}
	return &AppVersionHandler{
		appVersionSvc: appVersionSvc,
		uploadDir:     uploadDir,
		baseURL:       strings.TrimRight(baseURL, "/"),
	}
}

// CheckUpdate 检查应用更新
//
// GET /api/v1/check-update?platform=macos&currentVersion=1.0.0&buildNumber=1
//
// Response 200: 有新版本可用
// {
//   "version": "1.1.0",
//   "buildNumber": 2,
//   "description": "Bug fixes and improvements",
//   "isMandatory": false,
//   "releaseDate": "2026-03-24T10:30:00Z",
//   "downloadUrl": "https://release.shepaw.com/download/...",
//   "fileSize": 12345678,
//   ...
// }
//
// Response 204: 已是最新版本（无响应体）
//
// Response 400: 参数错误
// {
//   "error": "invalid parameters"
// }
func (h *AppVersionHandler) CheckUpdate(c *gin.Context) {
	var req models.CheckUpdateRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid parameters"})
		return
	}

	// 调用服务检查更新
	appVersion, err := h.appVersionSvc.CheckForUpdate(
		req.Platform,
		req.CurrentVersion,
		req.BuildNumber,
	)
	if err != nil {
		// 日志记录错误（生产环境应集成日志服务）
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check update"})
		return
	}

	// 无更新可用
	if appVersion == nil {
		c.AbortWithStatus(http.StatusNoContent)
		return
	}

	// 返回版本信息
	response := models.CheckUpdateResponse{
		Version:           appVersion.Version,
		BuildNumber:       appVersion.BuildNumber,
		Description:       appVersion.Description,
		IsMandatory:       appVersion.IsMandatory,
		ReleaseDate:       appVersion.ReleaseDate.Format("2006-01-02T15:04:05Z07:00"),
		DownloadURL:       appVersion.DownloadURL,
		FileSize:          appVersion.FileSize,
		Checksum:          appVersion.Checksum,
		MinIOSVersion:     appVersion.MinIOSVersion,
		MinAndroidSDK:     appVersion.MinAndroidSDK,
		MinMacOSVersion:   appVersion.MinMacOSVersion,
		MinWindowsVersion: appVersion.MinWindowsVer,
	}

	c.JSON(http.StatusOK, response)
}

// AdminCreateVersion 管理员创建/更新版本（需要认证）
// POST /admin/api/app-versions
func (h *AppVersionHandler) AdminCreateVersion(c *gin.Context) {
	var req models.AppVersion
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	// 验证必填字段
	if req.Platform == "" || req.Version == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "platform and version are required"})
		return
	}

	if err := h.appVersionSvc.CreateOrUpdateVersion(&req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create/update version"})
		return
	}

	c.JSON(http.StatusCreated, req)
}

// AdminListVersions 管理员列出所有版本（需要认证）
// GET /admin/api/app-versions?platform=macos
func (h *AppVersionHandler) AdminListVersions(c *gin.Context) {
	platform := c.Query("platform")
	versions, err := h.appVersionSvc.ListVersions(platform)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list versions"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"versions": versions})
}

// AdminDeleteVersion 管理员删除版本（需要认证）
// DELETE /admin/api/app-versions/:id
func (h *AppVersionHandler) AdminDeleteVersion(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "version id is required"})
		return
	}

	if err := h.appVersionSvc.DeleteVersion(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete version"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "version deleted"})
}

// AdminUploadVersionFile 管理员上传版本安装包（需要认证）
//
// POST /admin/api/app-versions/upload
// Content-Type: multipart/form-data
// 字段：file（安装包文件）, platform（平台名，用于子目录分类）
//
// Response 200:
//
//	{
//	  "downloadUrl": "/uploads/versions/macos/1713600000_MyApp.dmg",
//	  "fileSize": 52428800,
//	  "checksum": "sha256:abc123..."
//	}
//
// 限制：单文件最大 500 MB
func (h *AppVersionHandler) AdminUploadVersionFile(c *gin.Context) {
	const maxUploadSize = 500 << 20 // 500 MB

	// 限制读取大小，防止内存耗尽
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadSize)

	// 解析 multipart（内存缓冲 32 MB，其余写临时文件）
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "文件过大或请求格式错误，单文件最大 500 MB"})
		return
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择要上传的文件"})
		return
	}

	platform := strings.TrimSpace(c.PostForm("platform"))
	if platform == "" {
		platform = "common"
	}

	// 校验平台值，防止路径穿越
	validPlatforms := map[string]bool{
		"ios": true, "android": true, "macos": true,
		"windows": true, "linux": true, "common": true,
	}
	if !validPlatforms[platform] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的平台参数"})
		return
	}

	// 确保目标目录存在
	destDir := filepath.Join(h.uploadDir, platform)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器存储目录创建失败"})
		return
	}

	// 构造目标文件名：时间戳 + 原始文件名，避免冲突
	origName := filepath.Base(fileHeader.Filename)
	// 只保留安全字符，防止目录穿越
	origName = sanitizeFilename(origName)
	destName := fmt.Sprintf("%d_%s", time.Now().Unix(), origName)
	destPath := filepath.Join(destDir, destName)

	// 打开上传文件
	src, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取上传文件失败"})
		return
	}
	defer src.Close()

	// 创建目标文件
	dst, err := os.Create(destPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存文件失败"})
		return
	}
	defer dst.Close()

	// 流式写入并同时计算 SHA256
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(dst, hash), src)
	if err != nil {
		os.Remove(destPath) // 写入失败则删除不完整文件
		c.JSON(http.StatusInternalServerError, gin.H{"error": "文件写入失败"})
		return
	}

	checksum := fmt.Sprintf("sha256:%x", hash.Sum(nil))

	// 构造公开访问 URL
	// 相对路径如 /uploads/versions/macos/1713600000_MyApp.dmg
	relPath := fmt.Sprintf("/uploads/versions/%s/%s", platform, destName)
	downloadURL := relPath
	if h.baseURL != "" {
		downloadURL = h.baseURL + relPath
	}

	c.JSON(http.StatusOK, gin.H{
		"downloadUrl": downloadURL,
		"fileSize":    written,
		"checksum":    checksum,
		"filename":    destName,
	})
}

// sanitizeFilename 移除文件名中的路径分隔符和其他危险字符，只保留安全字符
func sanitizeFilename(name string) string {
	// 去掉路径分量
	name = filepath.Base(name)
	// 只保留字母、数字、连字符、下划线、点
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	result := b.String()
	if result == "" || result == "." {
		result = "upload"
	}
	return result
}
