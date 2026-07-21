package models

import "gorm.io/gorm"

type Comment struct {
	gorm.Model
	Content   string   `binding:"required" gorm:"type:text"`
	UserID    uint     `gorm:"index"`
	User      User     `gorm:"foreignKey:UserID"`
	ArticleID uint     `gorm:"index"`
	Article   Article  `gorm:"foreignKey:ArticleID"`
	ParentID  *uint    `gorm:"index"`
	Parent    *Comment `gorm:"foreignKey:ParentID"`
	LikeCount int64    `gorm:"default:0"`
}
