-- 应用版本表初始化示例数据
-- 可直接执行此 SQL 在测试环境中插入版本信息

-- 清除现有数据（仅用于测试）
DELETE FROM app_versions WHERE platform IN ('ios', 'android', 'macos', 'windows', 'linux');

-- macOS 版本示例
INSERT INTO app_versions (id, version, build_number, platform, description, is_mandatory, release_date, download_url, file_size, checksum, min_mac_osversion, active, created_at, updated_at) VALUES
('av_macos_1', '1.1.0', 2, 'macos', 'Bug fixes and performance improvements', false, now(), 'https://release.shepaw.com/download/Paw-1.1.0.dmg', 52428800, 'sha256:abc123...', '11.0', true, now(), now());

-- Windows 版本示例
INSERT INTO app_versions (id, version, build_number, platform, description, is_mandatory, release_date, download_url, file_size, checksum, min_windows_version, active, created_at, updated_at) VALUES
('av_windows_1', '1.1.0', 2, 'windows', 'Bug fixes and performance improvements', false, now(), 'https://release.shepaw.com/download/Paw-1.1.0.exe', 67108864, 'sha256:def456...', '10.0', true, now(), now());

-- iOS 版本示例
INSERT INTO app_versions (id, version, build_number, platform, description, is_mandatory, release_date, download_url, file_size, checksum, min_ios_version, active, created_at, updated_at) VALUES
('av_ios_1', '1.1.0', 2, 'ios', 'Bug fixes and performance improvements', false, now(), 'https://apps.apple.com/app/paw/...', 0, 'sha256:ghi789...', '14.0', true, now(), now());

-- Android 版本示例
INSERT INTO app_versions (id, version, build_number, platform, description, is_mandatory, release_date, download_url, file_size, checksum, min_android_sdk, active, created_at, updated_at) VALUES
('av_android_1', '1.1.0', 2, 'android', 'Bug fixes and performance improvements', false, now(), 'https://play.google.com/store/apps/details?id=com.paw...', 0, 'sha256:jkl012...', 21, true, now(), now());

-- Linux 版本示例
INSERT INTO app_versions (id, version, build_number, platform, description, is_mandatory, release_date, download_url, file_size, checksum, active, created_at, updated_at) VALUES
('av_linux_1', '1.1.0', 2, 'linux', 'Bug fixes and performance improvements', false, now(), 'https://release.shepaw.com/download/paw-1.1.0-x86_64.AppImage', 45349376, 'sha256:mno345...', true, now(), now());

-- 强制更新示例（特定平台）
-- INSERT INTO app_versions (id, version, build_number, platform, description, is_mandatory, release_date, download_url, file_size, checksum, min_mac_osversion, active, created_at, updated_at) VALUES
-- ('av_macos_critical', '1.2.0', 3, 'macos', 'CRITICAL: Security update. All users must update immediately.', true, now(), 'https://release.shepaw.com/download/Paw-1.2.0.dmg', 52428800, 'sha256:critical...', '11.0', true, now(), now());
