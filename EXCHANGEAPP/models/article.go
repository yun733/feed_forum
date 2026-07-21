package models

import "gorm.io/gorm"

type Article struct {
	gorm.Model
	Title        string `binding:"required" gorm:"index;size:255"`
	Content      string `binding:"required" gorm:"type:longtext"`
	Preview      string `binding:"required" gorm:"size:500"`
	AuthorID     uint   `gorm:"index"`
	Author       User   `gorm:"foreignKey:AuthorID"`
	Status       string `gorm:"default:draft;size:20"` // draft, published
	ViewCount    int64  `gorm:"default:0"`
	Category     string `gorm:"size:50;index"`
	LikeCount    int64  `gorm:"default:0"`
	CommentCount int64  `gorm:"default:0"`
}
