package services

import (
	"fmt"
	"strings"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"gorm.io/gorm"
)

// AppVersionService 应用版本管理服务
type AppVersionService struct {
	db *gorm.DB
}

// NewAppVersionService 创建应用版本服务实例
func NewAppVersionService(db *gorm.DB) *AppVersionService {
	return &AppVersionService{db: db}
}

// VersionParts 版本号的三个部分
type VersionParts struct {
	Major int
	Minor int
	Patch int
}

// ParseVersion 解析版本字符串（格式：1.2.3）
func (s *AppVersionService) ParseVersion(version string) (*VersionParts, error) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid version format, expected X.Y.Z, got %s", version)
	}

	var vp VersionParts
	if _, err := fmt.Sscanf(parts[0], "%d", &vp.Major); err != nil {
		return nil, fmt.Errorf("invalid major version: %v", err)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &vp.Minor); err != nil {
		return nil, fmt.Errorf("invalid minor version: %v", err)
	}
	if _, err := fmt.Sscanf(parts[2], "%d", &vp.Patch); err != nil {
		return nil, fmt.Errorf("invalid patch version: %v", err)
	}

	return &vp, nil
}

// IsVersionLower 检查 v1 是否低于 v2
func (s *AppVersionService) IsVersionLower(v1, v2 *VersionParts) bool {
	if v1.Major != v2.Major {
		return v1.Major < v2.Major
	}
	if v1.Minor != v2.Minor {
		return v1.Minor < v2.Minor
	}
	return v1.Patch < v2.Patch
}

// CheckForUpdate 检查是否有新版本可用
// 返回值：
// - *models.AppVersion: 可用的新版本信息（nil 表示无更新）
// - error: 查询错误
func (s *AppVersionService) CheckForUpdate(platform string, currentVersion string, currentBuildNumber int) (*models.AppVersion, error) {
	// 解析当前版本
	currentVP, err := s.ParseVersion(currentVersion)
	if err != nil {
		return nil, err
	}

	// 从数据库获取该平台最新的活跃版本
	var appVersion models.AppVersion
	result := s.db.Where(
		"platform = ? AND active = ?",
		platform,
		true,
	).Order("build_number DESC").First(&appVersion)

	if result.Error != nil {
		if result.Error == gorm.ErrRecordNotFound {
			// 没有配置该平台的版本，视为无更新
			return nil, nil
		}
		return nil, result.Error
	}

	// 解析最新版本
	latestVP, err := s.ParseVersion(appVersion.Version)
	if err != nil {
		// 数据库中的版本格式错误，忽略
		return nil, nil
	}

	// 检查是否有更新
	// 1. 版本号更高，或
	// 2. 版本号相同但构建号更高
	hasVersionUpdate := s.IsVersionLower(currentVP, latestVP)
	hasBuildUpdate := currentVP == latestVP && currentBuildNumber < appVersion.BuildNumber

	if hasVersionUpdate || hasBuildUpdate {
		return &appVersion, nil
	}

	return nil, nil
}

// CreateOrUpdateVersion 创建或更新应用版本（管理员操作，后续实现）
func (s *AppVersionService) CreateOrUpdateVersion(av *models.AppVersion) error {
	if av.ID == "" {
		av.ID = fmt.Sprintf("av_%d_%s", time.Now().Unix(), av.Platform)
	}

	// 如果标记为 active，则将同平台其他版本改为 inactive
	if av.Active {
		if err := s.db.Model(&models.AppVersion{}).
			Where("platform = ? AND id != ?", av.Platform, av.ID).
			Update("active", false).Error; err != nil {
			return err
		}
	}

	return s.db.Save(av).Error
}

// GetVersionByID 根据 ID 获取版本信息
func (s *AppVersionService) GetVersionByID(id string) (*models.AppVersion, error) {
	var av models.AppVersion
	if err := s.db.First(&av, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &av, nil
}

// ListVersions 列出所有版本（平台可选过滤）
func (s *AppVersionService) ListVersions(platform string) ([]models.AppVersion, error) {
	var versions []models.AppVersion
	query := s.db
	if platform != "" {
		query = query.Where("platform = ?", platform)
	}
	if err := query.Order("platform, created_at DESC").Find(&versions).Error; err != nil {
		return nil, err
	}
	return versions, nil
}

// DeleteVersion 删除版本
func (s *AppVersionService) DeleteVersion(id string) error {
	return s.db.Delete(&models.AppVersion{}, "id = ?", id).Error
}
