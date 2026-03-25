package models

import (
	"time"
)

// 版本平台类型
const (
	PlatformIOS     = "ios"
	PlatformAndroid = "android"
	PlatformMacOS   = "macos"
	PlatformWindows = "windows"
	PlatformLinux   = "linux"
)

// AppVersion 应用版本信息（存储在数据库中）
type AppVersion struct {
	ID              string    `gorm:"primaryKey" json:"id"`
	Version         string    `json:"version"`         // 版本号 1.2.3
	BuildNumber     int       `json:"buildNumber"`     // 构建号
	Platform        string    `json:"platform"`        // ios/android/macos/windows/linux
	Description     string    `json:"description"`     // 更新描述
	IsMandatory     bool      `json:"isMandatory"`     // 是否强制更新
	ReleaseDate     time.Time `json:"releaseDate"`     // 发布时间
	DownloadURL     string    `json:"downloadUrl"`     // 下载链接
	FileSize        int64     `json:"fileSize"`        // 文件大小（字节）
	Checksum        string    `json:"checksum"`        // 校验和（sha256）
	MinIOSVersion   string    `json:"minIosVersion"`   // 支持的最低 iOS 版本
	MinAndroidSDK   int       `json:"minAndroidSdk"`   // 支持的最低 Android SDK
	MinMacOSVersion string    `json:"minMacOSVersion"` // 支持的最低 macOS 版本
	MinWindowsVer   string    `json:"minWindowsVersion"` // 支持的最低 Windows 版本
	Active          bool      `json:"active"`          // 是否为该平台的当前活跃版本
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// TableName 指定表名
func (AppVersion) TableName() string {
	return "app_versions"
}

// CheckUpdateRequest 检查更新的请求参数
type CheckUpdateRequest struct {
	Platform       string `form:"platform" binding:"required,oneof=ios android macos windows linux"`
	CurrentVersion string `form:"currentVersion" binding:"required"`     // 1.2.3 格式
	BuildNumber    int    `form:"buildNumber" binding:"required,min=0"` // >= 0
}

// CheckUpdateResponse 检查更新的响应
type CheckUpdateResponse struct {
	Version           string `json:"version"`           // 最新版本号 1.2.3
	BuildNumber       int    `json:"buildNumber"`       // 构建号
	Description       string `json:"description"`       // 更新内容
	IsMandatory       bool   `json:"isMandatory"`       // 是否强制更新
	ReleaseDate       string `json:"releaseDate"`       // ISO 8601 格式
	DownloadURL       string `json:"downloadUrl"`       // 下载链接
	FileSize          int64  `json:"fileSize,omitempty"`        // 文件大小（字节），可选
	Checksum          string `json:"checksum,omitempty"`        // 校验和，可选
	MinIOSVersion     string `json:"minIosVersion,omitempty"`
	MinAndroidSDK     int    `json:"minAndroidSdk,omitempty"`
	MinMacOSVersion   string `json:"minMacOSVersion,omitempty"`
	MinWindowsVersion string `json:"minWindowsVersion,omitempty"`
}
