package cache

import "sync"

type Call struct { done chan struct{} }
func (c *Call) Done() <-chan struct{} { return c.done }

type Coalescer struct { mu sync.Mutex; calls map[string]*Call }
func NewCoalescer() *Coalescer { return &Coalescer{calls: make(map[string]*Call)} }
// Start returns the in-flight call and whether the caller owns the fetch.
func (c *Coalescer) Start(key string) (*Call, bool) { c.mu.Lock(); defer c.mu.Unlock(); if call:=c.calls[key]; call!=nil { return call,false }; call:=&Call{done:make(chan struct{})}; c.calls[key]=call; return call,true }
func (c *Coalescer) Finish(key string, call *Call) { c.mu.Lock(); if c.calls[key]==call { delete(c.calls,key); close(call.done) }; c.mu.Unlock() }
