package atomic

import "sync/atomic"

// Int32 提供原子操作
type Int32 struct {
	v int32
}

// Add：计数增加 i ，返回新值。减操作可以为 ：Add(-1)
func (a *Int32) Add(i int32) int32 {
	return atomic.AddInt32(&a.v, i)
}

// Swap：交换值，并返回原来的值
func (a *Int32) Swap(i int32) int32 {
	return atomic.SwapInt32(&a.v, i)
}

// Get：获取值
func (a *Int32) Get() int32 {
	return atomic.LoadInt32(&a.v)
}

// Int64：提供原子操作
type Int64 struct {
	v int64
}

// Add：计数增加 i ，返回新值。减操作可以为 ：Add(-1)
func (a *Int64) Add(i int64) int64 {
	return atomic.AddInt64(&a.v, i)
}

// Swap：交换值，并返回原来的值
func (a *Int64) Swap(i int64) int64 {
	return atomic.SwapInt64(&a.v, i)
}

// Get：获取值
func (a *Int64) Get() int64 {
	return atomic.LoadInt64(&a.v)
}
