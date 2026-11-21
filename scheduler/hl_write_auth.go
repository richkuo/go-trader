package main

import (
	"errors"
	"fmt"
	"strings"
)

type hlWriteOp uint8

const (
	hlWriteExecute hlWriteOp = 1 << iota
	hlWriteClose
	hlWriteStop
	hlWriteProtection
	hlWriteLimitOpen
	hlWriteCancel
)

func (op hlWriteOp) String() string {
	switch op {
	case hlWriteExecute:
		return "execute"
	case hlWriteClose:
		return "close"
	case hlWriteStop:
		return "stop update"
	case hlWriteProtection:
		return "protection sync"
	case hlWriteLimitOpen:
		return "limit open"
	case hlWriteCancel:
		return "order cancel"
	}
	return fmt.Sprintf("op(%d)", uint8(op))
}

type hlWriteAuth struct {
	owner   string
	coin    string
	ops     hlWriteOp
	orderID int64
	account bool
}

type hlWriteRefusedError struct {
	op     hlWriteOp
	coin   string
	detail string
}

func (e *hlWriteRefusedError) Error() string {
	return fmt.Sprintf("Hyperliquid %s for %s refused before launch: %s; no order was sent", e.op, e.coin, e.detail)
}

func isHLWriteRefused(err error) bool {
	var refused *hlWriteRefusedError
	return errors.As(err, &refused)
}

func hlWriteCoinKey(coin string) string {
	return strings.ToUpper(strings.TrimSpace(coin))
}

func hlLiveWriteOwner(sc StrategyConfig) bool {
	if strings.TrimSpace(sc.ID) == "" || sc.Platform != "hyperliquid" {
		return false
	}
	if sc.Type != "perps" && sc.Type != "manual" {
		return false
	}
	return portfolioScopeFor(sc) == ScopeLive && partitionFor(sc).IsLive()
}

func hlStrategyWriteAuth(sc StrategyConfig, coin string, ops hlWriteOp) hlWriteAuth {
	if !hlLiveWriteOwner(sc) || ops == 0 {
		return hlWriteAuth{}
	}
	key := hlWriteCoinKey(coin)
	if key == "" || key != hlWriteCoinKey(hyperliquidConfiguredCoin(sc)) {
		return hlWriteAuth{}
	}
	return hlWriteAuth{owner: sc.ID, coin: key, ops: ops}
}

func hlHedgeWriteAuth(sc StrategyConfig, coin string, ops hlWriteOp) hlWriteAuth {
	if !hlLiveWriteOwner(sc) || sc.Type != "perps" || ops == 0 {
		return hlWriteAuth{}
	}
	key := hlWriteCoinKey(coin)
	hc := hedgeCoin(sc)
	if key == "" || hc == "" || key != hlWriteCoinKey(hc) {
		return hlWriteAuth{}
	}
	return hlWriteAuth{owner: sc.ID, coin: key, ops: ops}
}

func hlAccountRecoveryWriteAuth(coin string, ops hlWriteOp) hlWriteAuth {
	key := hlWriteCoinKey(coin)
	if key == "" || ops == 0 {
		return hlWriteAuth{}
	}
	return hlWriteAuth{owner: "account", coin: key, ops: ops, account: true}
}

func (a hlWriteAuth) forOrder(oid int64) hlWriteAuth {
	if oid <= 0 {
		return hlWriteAuth{}
	}
	a.orderID = oid
	return a
}

func (a hlWriteAuth) check(op hlWriteOp, coin string, oid int64) error {
	refuse := func(detail string) error {
		return &hlWriteRefusedError{op: op, coin: strings.TrimSpace(coin), detail: detail}
	}
	if a.owner == "" || a.ops == 0 {
		return refuse("no live Hyperliquid owner authorized this write")
	}
	if a.ops&op == 0 {
		return refuse(fmt.Sprintf("the authorization for %s does not cover this operation", a.owner))
	}
	if key := hlWriteCoinKey(coin); key == "" || key != a.coin {
		return refuse(fmt.Sprintf("the authorization for %s covers coin %s only", a.owner, a.coin))
	}
	if op == hlWriteCancel && (a.orderID <= 0 || a.orderID != oid) {
		return refuse(fmt.Sprintf("the authorization for %s does not cover order %d", a.owner, oid))
	}
	return nil
}
