package handlers

import (
	"net/http"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
)

// AppVersionHandler 应用版本相关的处理器
type AppVersionHandler struct {
	appVersionSvc *services.AppVersionService
}

// NewAppVersionHandler 创建应用版本处理器实例
func NewAppVersionHandler(appVersionSvc *services.AppVersionService) *AppVersionHandler {
	return &AppVersionHandler{
		appVersionSvc: appVersionSvc,
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
