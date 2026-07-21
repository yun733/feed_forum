package utils

import (
	"hash/fnv"

	"github.com/go-redis/redis/v8"
	"context"
)

// BloomFilter 基于 Redis 的布隆过滤器，用于 Feed 去重
type BloomFilter struct {
	client *redis.Client
	key    string
	hashes int
}

func NewBloomFilter(client *redis.Client, key string) *BloomFilter {
	return &BloomFilter{client: client, key: key, hashes: 5}
}

func (bf *BloomFilter) Add(ctx context.Context, item string) error {
	positions := bf.hashPositions(item)
	pipe := bf.client.Pipeline()
	for _, pos := range positions {
		pipe.SetBit(ctx, bf.key, pos, 1)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (bf *BloomFilter) Exists(ctx context.Context, item string) (bool, error) {
	positions := bf.hashPositions(item)
	pipe := bf.client.Pipeline()
	cmds := make([]*redis.IntCmd, len(positions))
	for i, pos := range positions {
		cmds[i] = pipe.GetBit(ctx, bf.key, pos)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}
	for _, cmd := range cmds {
		if cmd.Val() == 0 {
			return false, nil
		}
	}
	return true, nil
}

func (bf *BloomFilter) hashPositions(item string) []int64 {
	positions := make([]int64, bf.hashes)
	h := fnv.New64a()
	for i := 0; i < bf.hashes; i++ {
		h.Reset()
		h.Write([]byte{byte(i)})
		h.Write([]byte(item))
		positions[i] = int64(h.Sum64() % (1 << 20)) // 1M bits
	}
	return positions
}
