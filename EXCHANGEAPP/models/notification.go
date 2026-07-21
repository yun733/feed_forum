package models

import "gorm.io/gorm"

type Notification struct {
	gorm.Model
	UserID     uint   `gorm:"index"`
	FromUserID uint
	FromUser   User   `gorm:"foreignKey:FromUserID"`
	Type       string `gorm:"size:20"` // like, comment, follow
	TargetID   uint
	Content    string `gorm:"size:500"`
	IsRead     bool   `gorm:"default:false"`
}
