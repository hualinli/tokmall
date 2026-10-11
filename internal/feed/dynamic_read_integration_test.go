package feed

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/testsupport"
)

func TestPostgresDynamicReadPreservesCountsCardsAndFreshness(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))
	ctx := context.Background()
	require.NoError(t, db.Exec(`INSERT INTO users(id,email,username,password_hash) VALUES
		(1,'a@test.local','author','hash'),(2,'b@test.local','buyer','hash'),(3,'c@test.local','buyer2','hash')`).Error)
	require.NoError(t, db.Exec(`INSERT INTO videos(id,author_id,title,object_key,status,published_at,media_probe_status) VALUES
		(1,1,'one','one.mp4','PUBLISHED',now(),'SKIPPED'),
		(2,1,'two','two.mp4','PUBLISHED',now(),'SKIPPED'),
		(3,1,'hidden','hidden.mp4','PENDING_REVIEW',NULL,'SKIPPED')`).Error)
	require.NoError(t, db.Exec(`INSERT INTO likes(user_id,video_id) VALUES (2,1),(3,1),(2,2)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO products(id,merchant_id,title,status) VALUES
		(1,1,'many skus','ON_SALE'),(2,1,'off sale','OFF_SALE'),
		(3,1,'deleted','DELETED'),(4,1,'unlinked','ON_SALE')`).Error)
	require.NoError(t, db.Exec(`INSERT INTO skus(id,product_id,price,status) VALUES
		(1,1,400,'ACTIVE'),(2,1,200,'ACTIVE'),(3,1,1,'DISABLED'),(4,2,100,'DISABLED')`).Error)
	require.NoError(t, db.Exec(`INSERT INTO video_products(video_id,author_id,product_id,status,sort_order) VALUES
		(1,1,1,'ACTIVE',2),(1,1,2,'ACTIVE',1),(1,1,3,'ACTIVE',0),
		(1,1,4,'DISABLED',0),(3,1,1,'ACTIVE',0)`).Error)
	reader := NewDynamicReader(db)
	counts, cards, err := reader.ReadDynamic(ctx, []int64{1, 2, 3, 999})
	require.NoError(t, err)
	require.Equal(t, map[int64]int64{1: 2, 2: 1, 3: 0}, counts, "多个商品和 SKU 不放大点赞数")
	wantCards, err := NewProductCardReader(db).ListProductCards(ctx, []int64{1, 2, 3, 999})
	require.NoError(t, err)
	require.Equal(t, wantCards, cards, "过滤与排序沿用原读模型")
	require.Len(t, cards, 2)
	require.Equal(t, int64(2), cards[0].ProductID)
	require.Equal(t, "OFF_SALE", cards[0].ProductStatus, "下架卡仍展示状态")
	require.Zero(t, cards[0].MinPrice, "无启用 SKU 时价格为零")
	require.Equal(t, int64(200), cards[1].MinPrice)

	cache := &fakeCache{}
	svc := NewService(NewRepository(db), NewCodec("integration-cursor-secret"), fakeURLProvider{}, cache, nil, nil)
	svc.SetDynamicReader(reader)
	first, err := svc.List(ctx, 0, "")
	require.NoError(t, err)
	find := func(result *FeedResult, id int64) FeedItem {
		for _, item := range result.Items {
			if item.VideoID == id {
				return item
			}
		}
		t.Fatalf("video %d missing", id)
		return FeedItem{}
	}
	require.Equal(t, int64(2), find(first, 1).LikeCount)
	require.Len(t, find(first, 1).ProductCards, 2)
	for _, item := range cache.store.Items {
		require.Zero(t, item.LikeCount)
		require.Empty(t, item.ProductCards)
	}
	require.NoError(t, db.Exec("DELETE FROM likes WHERE user_id=2 AND video_id=1").Error)
	require.NoError(t, db.Exec("UPDATE video_products SET status='DISABLED' WHERE video_id=1 AND product_id=2").Error)
	require.NoError(t, db.Exec("UPDATE skus SET price=150 WHERE id=2").Error)
	second, err := svc.List(ctx, 0, "")
	require.NoError(t, err)
	require.Equal(t, int64(1), find(second, 1).LikeCount)
	require.Len(t, find(second, 1).ProductCards, 1)
	require.Equal(t, int64(150), find(second, 1).ProductCards[0].MinPrice)
	require.NotNil(t, find(second, 2).ProductCards, "空卡列表仍返回数组")
	require.Equal(t, int64(2), find(first, 1).LikeCount, "请求结果不共享动态字段")
}
