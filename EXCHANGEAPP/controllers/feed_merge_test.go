package controllers

import (
	"encoding/base64"
	"exchangeapp/models"
	"testing"
	"time"
)

func article(id uint, ts int64) models.Article {
	a := models.Article{}
	a.ID = id
	a.CreatedAt = time.Unix(ts, 0)
	return a
}

// 归并的两个来源（timeline 推送 / outbox 归并）语义上不相交，
// 但迁移窗口、网络重试、MQ 重复投递都可能让同一篇文章出现两次。
// 这里断言的是「设计上不产生重复」之外的兜底：真重复了也必须只出现一次。
func TestMergeAndTrimDedupAndOrder(t *testing.T) {
	now := time.Now().Unix()
	articles := []models.Article{
		article(1, now-100), // 来自 timeline
		article(2, now-50),  // 来自 outbox（更新）
		article(1, now-100), // 重复投递
		article(3, now-200),
		article(0, 0), // 零值：Redis 里查不到对应行时留下占位
	}

	got := mergeAndTrim(articles, map[uint]bool{}, 10)

	if len(got) != 3 {
		t.Fatalf("expected 3 unique non-zero articles, got %d: %+v", len(got), got)
	}

	want := []uint{2, 1, 3}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("position %d: got id=%d, want id=%d (order must be desc by time)", i, got[i].ID, w)
		}
	}
}

func TestMergeAndTrimFiltersRead(t *testing.T) {
	now := time.Now().Unix()
	articles := []models.Article{article(1, now), article(2, now-1), article(3, now-2)}

	got := mergeAndTrim(articles, map[uint]bool{2: true}, 10)

	if len(got) != 2 {
		t.Fatalf("expected 2 articles after read-filtering, got %d", len(got))
	}
	for _, a := range got {
		if a.ID == 2 {
			t.Error("already-read article 2 should have been filtered out")
		}
	}
}

func TestMergeAndTrimTruncatesToOnePage(t *testing.T) {
	now := time.Now().Unix()
	var articles []models.Article
	for i := 1; i <= 20; i++ {
		articles = append(articles, article(uint(i), now-int64(i)))
	}

	got := mergeAndTrim(articles, map[uint]bool{}, 5)
	if len(got) != 5 {
		t.Fatalf("expected page of 5, got %d", len(got))
	}
	// 截断必须保留最新的 5 条
	for i, a := range got {
		if want := uint(i + 1); a.ID != want {
			t.Errorf("position %d: got id=%d, want id=%d", i, a.ID, want)
		}
	}
}

func TestMergeAndTrimEmpty(t *testing.T) {
	if got := mergeAndTrim(nil, map[uint]bool{}, 10); got != nil {
		t.Errorf("nil input should stay nil, got %+v", got)
	}
}

// 游标是分页正确性的地基：编码/解码必须无损，否则翻页会跳条目或死循环
func TestCursorRoundTrip(t *testing.T) {
	original := FeedCursor{Score: 1750000000, ArticleID: 424242}

	decoded, err := DecodeCursor(original.Encode())
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if decoded.Score != original.Score || decoded.ArticleID != original.ArticleID {
		t.Errorf("round trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"not-base64!!!", base64.StdEncoding.EncodeToString([]byte("nocolon"))} {
		if _, err := DecodeCursor(bad); err == nil {
			t.Errorf("DecodeCursor(%q) should have failed", bad)
		}
	}
}
