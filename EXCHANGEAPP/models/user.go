package models

import (
	"time"

	"gorm.io/gorm"
)

type User struct {
	gorm.Model
	Username     string    `gorm:"size:255;uniqueIndex"`
	Password     string    `gorm:"size:255;not null"`
	Avatar       string    `gorm:"size:512"`
	Bio          string    `gorm:"size:500"`
	LastActiveAt time.Time
}
