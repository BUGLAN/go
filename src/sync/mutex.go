// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
//互斥锁是一种互斥锁。
//互斥锁的零值是无锁定的互斥锁。
//
//互斥锁在第一次使用后不能复制。
type Mutex struct {
	state int32  // 表示锁的状态
	sema  uint32 // 信号量
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked      = 1 << iota // mutex is locked
	mutexWoken                   // 醒来2
	mutexStarving                // 饥饿的 4
	mutexWaiterShift = iota      // 3

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	//互斥公平。
	//
	//互斥可以有两种操作模式:正常和饥饿。
	//在正常模式下，侍者按FIFO顺序排队，但是唤醒的侍者会被唤醒
	//不拥有互斥锁，并与新到达的goroutines竞争
	//所有权。新来的goroutines有一个优势——他们确实是
	//已经在CPU上运行，可能有很多，所以a唤醒
	//服务员很有可能会输。在这种情况下，它被排在前面
	//等待队列的。如果服务员在超过1毫秒的时间内无法获得互斥，
	//它将互斥锁切换到饥饿模式。
	//
	//在饥饿模式下，互斥锁的所有权直接从
	//打开goroutine给排在队伍前面的服务员。
	//新到达的goroutines不会尝试获取互斥锁，即使它出现了
	//要解锁，不要尝试旋转。相反，他们排队等候
	//等待队列的尾部。
	//
	//如果一个服务员接收到互斥锁的所有权并看到它
	//(1)他是队伍里最后一个服务员，或者(2)他等了不到1毫秒，
	//它将互斥锁切换回正常操作模式。
	//
	//正常模式比goroutine的性能要好得多
	//即使有被阻塞的等待者，也会连续多次使用互斥锁。
	//饥饿模式对预防尾部潜伏期的病理病例很重要。
	starvationThresholdNs = 1e6
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	// cas 操作 如果stage是空闲的, 则原子性的修改状态
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	m.lockSlow()
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64 // 当前goroutine等待的时间
	starving := false       // 是否饥饿
	awoke := false          // 是否已唤醒
	iter := 0               // 循环次数
	old := m.state          // 当前锁的状态
	// 开始自旋
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		//不要在“饥饿”模式下旋转，所有权会传给侍者
		//所以无论如何我们都无法获得互斥锁。
		// 当 mutex 处于正常工作模式且能够自旋的时候，进行自旋操作（汇编实现，内部持续调用 PAUSE 指令，消耗 CPU 时间）
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			//主动旋转是有意义的。
			//尝试设置mutex唤醒标志来通知解锁
			//不唤醒其他被屏蔽的goroutines。
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			// 自旋
			runtime_doSpin()
			iter++ // 循环次数加1
			// 重新更新所得状态
			old = m.state
			continue
		}
		// 复制一份当前的状态，目的是根据当前状态设置出期望的状态，存在new里面，
		// 并且通过CAS来比较以及更新锁的状态
		// old用来存锁的当前状态
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		// 当 mutex 不处于饥饿状态的时候，将 new 的第一位设置为1，即加锁
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}

		// 当 mutex 处于加锁状态或饥饿状态的时候，新到来的 goroutine 进入等待队列
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// 当前 goroutine 将 mutex 切换为饥饿状态，但如果当前 mutex 未加锁，则不需要切换
		// Unlock 操作希望饥饿模式存在等待者
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			// 当前 goroutine 被唤醒，将 mutex.state 倒数第二位重置
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken
		}

		// 通过cas来设置所得状态
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// mutex 处于未加锁，正常模式下，当前 goroutine 获得锁
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}

			// 将被唤醒却没得到锁的 goroutine 插入当前等待队列的最前端
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving
				}
				// 更新状态
				atomic.AddInt32(&m.state, delta)
				break
			}
			awoke = true
			iter = 0
		} else {
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	// mutex 的 state 减去1，加锁状态 -> 未加锁
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	// 没lock直接unlock, panic
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	// 正常模式
	if new&mutexStarving == 0 {
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			// 如果没有等待者，或者已经存在一个 goroutine 被唤醒或得到锁，或处于饥饿模式
			// 无需唤醒任何处于等待状态的 goroutine
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			// 等待者数量减1，并将唤醒位改成1
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				// 唤醒一个阻塞的 goroutine，但不是唤醒第一个等待者
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.\
		// mutex 饥饿模式，直接将 mutex 拥有权移交给等待队列最前端的 goroutine
		runtime_Semrelease(&m.sema, true, 1)
	}
}
