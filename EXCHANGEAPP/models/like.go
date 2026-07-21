package models

import "gorm.io/gorm"

type Like struct {
	gorm.Model
	UserID     uint   `gorm:"uniqueIndex:idx_user_target"`
	TargetType string `gorm:"uniqueIndex:idx_user_target;size:20"` // "article" or "comment"
	TargetID   uint   `gorm:"uniqueIndex:idx_user_target"`
}
