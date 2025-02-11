package memdb

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/eleme/lindb/models"
	"github.com/eleme/lindb/pkg/field"
	"github.com/eleme/lindb/pkg/hashers"
	"github.com/eleme/lindb/pkg/interval"
	"github.com/eleme/lindb/pkg/timeutil"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
)

func Test_NewMemoryDatabase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := NewMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	assert.NotNil(t, md)
}

func Test_getBucket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	for i := 0; i < 1000; i++ {
		assert.NotNil(t, md.getBucket(hashers.Fnv32a(strconv.Itoa(i))))
	}
}

func Test_getOrCreateMStore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	for i := 0; i < 1000; i++ {
		assert.NotNil(t, md.getOrCreateMStore(strconv.Itoa(i)))
	}
}

func Test_setLimitations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	limitations := map[string]uint32{"cpu.load": 10, "memory": 100}
	md.getOrCreateMStore("cpu.load")
	md.getOrCreateMStore("loadavg")

	md.setLimitations(limitations)
	assert.Equal(t, uint32(10), md.getOrCreateMStore("cpu.load").getMaxTagsLimit())
	assert.NotEqual(t, uint32(10), md.getOrCreateMStore("loadavg").getMaxTagsLimit())
}

func Test_WithMaxTagsLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	md.getOrCreateMStore("cpu.load")
	limitationCh := make(chan map[string]uint32)
	md.WithMaxTagsLimit(limitationCh)
	md.WithMaxTagsLimit(limitationCh)

	limitationCh <- nil
	limitationCh <- map[string]uint32{"cpu.load": 10}
	time.Sleep(time.Millisecond * 10)
	assert.Equal(t, uint32(10), md.getOrCreateMStore("cpu.load").getMaxTagsLimit())

	close(limitationCh)
}

func Test_Write(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	assert.NotNil(t, md.Write(nil))

	p := models.NewMockPoint(ctrl)
	p.EXPECT().Name().Return("cpu.load").AnyTimes()
	p.EXPECT().Tags().Return("idle").AnyTimes()
	p.EXPECT().Timestamp().Return(timeutil.Now()).AnyTimes()
	p.EXPECT().Fields().Return(nil).Times(1)
	assert.NotNil(t, md.Write(p))

	fakeFields := make(map[string]models.Field)
	fakeField := models.NewMockField(ctrl)
	fakeField.EXPECT().Type().Return(field.MaxField).AnyTimes()
	fakeField.EXPECT().IsComplex().Return(true).AnyTimes()
	fakeFields["test"] = fakeField
	p.EXPECT().Fields().Return(fakeFields).AnyTimes()

	assert.Nil(t, md.Write(p))
	// assert error
	mStore := md.getOrCreateMStore("cpu.load")

	for i := 0; i < 110000; i++ {
		mStore.getOrCreateTSStore(strconv.Itoa(i))
	}
	assert.Equal(t, models.ErrTooManyTags, md.Write(p))
}

func Test_evict(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	setTagsIDTTL(60 * 1000) // 60 s
	for i := 0; i < 1000; i++ {
		md.getOrCreateMStore(strconv.Itoa(i))
	}
	for _, store := range md.mStoresList {
		md.evict(store)
	}
	// purges all
	assert.Equal(t, 0, len(md.mStoresList[0].m))
}

func Test_evictor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)
	md.evictNotifier <- struct{}{}
	md.evictNotifier <- struct{}{}
	md.evictNotifier <- struct{}{}
	time.Sleep(time.Millisecond * 100)
}

func Test_flushFamilyTo(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	gen := makeMockIDGenerator(ctrl)
	tw := makeMockTableWriter(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)
	md.generator = gen

	getMStore := func() *metricStore {
		mStore := newMetricStore("cpu")
		mStore.mutable = newVersionedTSMap()
		mStore.immutable = append(mStore.immutable, newVersionedTSMap())
		return mStore
	}
	md.mStoresList[0].m[hashers.Fnv32a("cpu")] = getMStore()
	assert.Nil(t, md.flushFamilyTo(1, tw))
}

func Test_ResetMetricStore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	mStore := md.getOrCreateMStore("cpu")
	assert.NotNil(t, md.ResetMetricStore("cpu"))

	mStore.mutable.version -= int64(time.Hour)
	assert.Nil(t, md.ResetMetricStore("cpu"))
}

func Test_CountMetrics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	for i := 0; i < 100; i++ {
		md.getOrCreateMStore(strconv.Itoa(i))
	}
	assert.Equal(t, 100, md.CountMetrics())
}

func Test_CountTags(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	mStore := md.getOrCreateMStore("cpu")
	for i := 0; i < 100; i++ {
		mStore.getOrCreateTSStore(strconv.Itoa(i))
	}
	assert.Equal(t, 100, md.CountTags("cpu"))
	assert.Equal(t, -1, md.CountTags("memory"))

}

func Test_Families(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	mStore := md.getOrCreateMStore("cpu")
	vm := newVersionedTSMap()
	vm.familyTimes = map[int64]struct{}{2: {}, 4: {}}
	mStore.mutable = vm

	assert.Len(t, md.Families(), 2)
}

func Test_IDSyner(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockGen := makeMockIDGenerator(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)
	md.generator = mockGen
	go md.IDSyncer(ctx, time.Millisecond)
	time.Sleep(time.Millisecond * 10)
}

func Test_syncID(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockGen := makeMockIDGenerator(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md, _ := newMemoryDatabase(ctx, 32, 10*1000, interval.Day)

	md.getOrCreateMStore("cpu").
		getOrCreateTSStore("host=alpha").
		getOrCreateFStore("idel", field.SumField)
	md.generator = mockGen
	md.syncID()
}
