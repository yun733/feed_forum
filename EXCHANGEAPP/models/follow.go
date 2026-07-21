package models

import "gorm.io/gorm"

type Follow struct {
	gorm.Model
	FollowerID  uint `gorm:"uniqueIndex:idx_follow"`
	FollowingID uint `gorm:"uniqueIndex:idx_follow"`
	IsMutual    bool `gorm:"default:false"`
}
