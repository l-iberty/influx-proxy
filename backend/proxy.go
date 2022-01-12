// Copyright 2021 Shiwen Cheng. All rights reserved.
// Use of this source code is governed by a MIT
// license that can be found in the LICENSE file.

package backend

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chengshiwen/influx-proxy/util"
)

type Proxy struct {
	sync.RWMutex
	Circles      []*Circle
	DBSet        util.Set
	CircleKeyMap map[int]string
}

func NewProxy(cfg *ProxyConfig) (ip *Proxy) {
	ip = &Proxy{
		Circles:      make([]*Circle, len(cfg.Circles)),
		DBSet:        util.NewSet(),
		CircleKeyMap: make(map[int]string),
	}
	for idx, circfg := range cfg.Circles {
		ip.Circles[idx] = NewCircle(circfg, cfg, idx)
		ip.CircleKeyMap[idx] = ""
	}
	for _, db := range cfg.DBList {
		ip.DBSet.Add(db)
	}
	rand.Seed(time.Now().Unix())
	return
}

func GetKey(db, meas string) string {
	var b strings.Builder
	b.Grow(len(db) + len(meas) + 1)
	b.WriteString(db)
	b.WriteString(",")
	b.WriteString(meas)
	return b.String()
}

// func (ip *Proxy) GetBackends(key string) []*Backend {
// 	backends := make([]*Backend, len(ip.Circles))
// 	for i, circle := range ip.Circles {
// 		backends[i] = circle.GetBackend(key)
// 	}
// 	return backends
// }

// 一个 key 只映射到一个 circle
func (ip *Proxy) GetBackends(key string) []*Backend {
	circle := ip.AssignCircle(key)
	backends := make([]*Backend, 1)
	backends[0] = circle.GetBackend(key)
	return backends
}

func (ip *Proxy) AssignCircle(key string) *Circle {
	ip.Lock()
	defer ip.Unlock()

	for i, k := range ip.CircleKeyMap {
		if k == key {
			return ip.Circles[i]
		}
	}
	for i, k := range ip.CircleKeyMap {
		if k == "" {
			ip.CircleKeyMap[i] = key
			return ip.Circles[i]
		}
	}
	return ip.Circles[rand.Intn(len(ip.Circles))]
}

func (ip *Proxy) GetCircle(key string) *Circle {
	ip.RLock()
	defer ip.RUnlock()

	for i, k := range ip.CircleKeyMap {
		if k == key {
			return ip.Circles[i]
		}
	}
	return nil
}

func (ip *Proxy) GetHealth(stats bool) []interface{} {
	var wg sync.WaitGroup
	health := make([]interface{}, len(ip.Circles))
	for i, c := range ip.Circles {
		wg.Add(1)
		go func(i int, c *Circle) {
			defer wg.Done()
			health[i] = c.GetHealth(stats)
		}(i, c)
	}
	wg.Wait()
	return health
}

func (ip *Proxy) Query(w http.ResponseWriter, req *http.Request) (body []byte, err error) {
	q := strings.TrimSpace(req.FormValue("q"))
	if q == "" {
		return nil, ErrEmptyQuery
	}

	tokens, check, from := CheckQuery(q)
	if !check {
		return nil, ErrIllegalQL
	}

	checkDb, showDb, alterDb, db := CheckDatabaseFromTokens(tokens)
	if !checkDb {
		db = req.FormValue("db")
		if db == "" {
			db, _ = GetDatabaseFromTokens(tokens)
		}
	}
	if !showDb {
		if db == "" {
			return nil, ErrDatabaseNotFound
		}
		if db == "_internal" || (len(ip.DBSet) > 0 && !ip.DBSet[db]) {
			return nil, fmt.Errorf("database forbidden: %s", db)
		}
	}

	selectOrShow := CheckSelectOrShowFromTokens(tokens)
	if selectOrShow && from {
		return QueryFromQL(w, req, ip, tokens, db)
	} else if selectOrShow && !from {
		return QueryShowQL(w, req, ip, tokens)
	} else if CheckDeleteOrDropMeasurementFromTokens(tokens) {
		return QueryDeleteOrDropQL(w, req, ip, tokens, db)
	} else if alterDb || CheckRetentionPolicyFromTokens(tokens) {
		return QueryAlterQL(w, req, ip)
	}
	return nil, ErrIllegalQL
}

func (ip *Proxy) Write(p []byte, db, precision string) (err error) {
	buf := bytes.NewBuffer(p)
	var line []byte
	for {
		line, err = buf.ReadBytes('\n')
		switch err {
		default:
			log.Printf("error: %s", err)
			return
		case io.EOF, nil:
			err = nil
		}
		if len(line) == 0 {
			break
		}
		ip.WriteRow(line, db, precision)
	}
	return
}

func (ip *Proxy) WriteRow(line []byte, db, precision string) {
	nanoLine := AppendNano(line, precision)
	meas, err := ScanKey(nanoLine)
	if err != nil {
		log.Printf("scan key error: %s", err)
		return
	}
	if !RapidCheck(nanoLine[len(meas):]) {
		log.Printf("invalid format, drop data: %s %s %s", db, precision, string(line))
		return
	}

	key := GetKey(db, meas)
	backends := ip.GetBackends(key)
	if len(backends) == 0 {
		log.Printf("write data error: can't get backends")
		return
	}

	point := &LinePoint{db, nanoLine}
	for _, be := range backends {
		err := be.WritePoint(point)
		if err != nil {
			log.Printf("write data to buffer error: %s, %s, %s, %s, %s", err, be.Url, db, precision, string(line))
		}
	}
}
