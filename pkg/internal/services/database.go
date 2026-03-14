package services

import (
	"fmt"
	"log"
	"os"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DatabaseService 封装数据库连接
type DatabaseService struct {
	DB *gorm.DB
}

// NewDatabaseService 创建数据库连接，支持 postgres 和 sqlite（URL 以 "sqlite:" 开头）
func NewDatabaseService(dbURL string) (*DatabaseService, error) {
	var dialector gorm.Dialector
	logLevel := logger.Silent
	if os.Getenv("DB_DEBUG") != "" {
		logLevel = logger.Info
	}

	if len(dbURL) > 7 && dbURL[:7] == "sqlite:" {
		// sqlite:./data.db  或  sqlite::memory:
		path := dbURL[7:]
		dialector = sqlite.Open(path)
	} else {
		dialector = postgres.Open(dbURL)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %v", err)
	}

	// 自动迁移所有模型
	err = db.AutoMigrate(
		&models.User{},
		&models.AccessToken{},
		&models.Channel{},
		&models.UserChannel{},
		&models.RateLimitRule{},
		&models.EmailVerification{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to auto migrate: %v", err)
	}

	log.Println("✅ Database connected and migrated")
	return &DatabaseService{DB: db}, nil
}

// Close 关闭数据库连接
func (s *DatabaseService) Close() error {
	sqlDB, err := s.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
