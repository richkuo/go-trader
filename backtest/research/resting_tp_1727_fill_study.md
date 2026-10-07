# Resting take-profit fill study

Read-only measurement for issue 1727. The capture tool and this study stay outside the scheduler. The study reads a frozen ledger export, a capture bundle, and a hash-checked candle manifest. It submits no order and writes no config or state.

Placement time, terminal time, requested size and limit price come only from venue order records. Export timestamps only attribute an order to a position. A missing venue field is `lifetime_unknown`. Tier prices, manual additions and replacement orders are not rebuilt from current configuration.

A bar that overlaps placement, cancellation or replacement is unknown. The bar that holds a venue fill is scored with that fill. A rule that predicts a full quantity against a venue partial fill is a quantity error. An unconfirmed candle basis or cancel-time source is a blocker. Raising k does not repair unknown placement timing.

The frozen manifest interval is 5m because the manifest verifier has no 1m interval. Live capture still asks for 1m candles. Venue field shapes were not confirmed on the venue in this run.

## Sample

```json
{
  "exclusions": [
    {
      "order_ordinal": 7,
      "reason": "boundary_unknown"
    },
    {
      "order_ordinal": 8,
      "reason": "shared_coin_ambiguous"
    },
    {
      "order_ordinal": 9,
      "reason": "limit_off_grid"
    },
    {
      "order_ordinal": 11,
      "reason": "acquisition_incomplete"
    }
  ],
  "orders_per_class": {
    "full_fill": 2,
    "lifetime_unknown": 1,
    "no_fill_touch": 1,
    "no_fill_trade_through": 1,
    "not_reached": 1,
    "partial_fill": 1
  },
  "orders_per_class_by_side": {
    "long": {
      "full_fill": 2,
      "lifetime_unknown": 1,
      "no_fill_touch": 1,
      "no_fill_trade_through": 1,
      "not_reached": 1,
      "partial_fill": 1
    }
  },
  "strategies": [
    "strat-a",
    "strat-b"
  ],
  "windows": [
    {
      "close_ms": 1704068700000,
      "coin": "ETH",
      "open_ms": 1704067800000,
      "side": "long",
      "strategy": "strat-a"
    },
    {
      "close_ms": 1704069900000,
      "coin": "ETH",
      "open_ms": 1704069000000,
      "side": "long",
      "strategy": "strat-a"
    },
    {
      "close_ms": 1704071100000,
      "coin": "ETH",
      "open_ms": 1704070200000,
      "side": "long",
      "strategy": "strat-a"
    },
    {
      "close_ms": 1704072300000,
      "coin": "ETH",
      "open_ms": 1704071400000,
      "side": "long",
      "strategy": "strat-a"
    },
    {
      "close_ms": 1704073500000,
      "coin": "ETH",
      "open_ms": 1704072600000,
      "side": "long",
      "strategy": "strat-a"
    },
    {
      "close_ms": 1704074400000,
      "coin": "ETH",
      "open_ms": 1704073800000,
      "side": "long",
      "strategy": "strat-a"
    },
    {
      "close_ms": 1704075900000,
      "coin": "ETH",
      "open_ms": 1704075000000,
      "side": "long",
      "strategy": "strat-a"
    },
    {
      "close_ms": 1704077100000,
      "coin": "ETH",
      "open_ms": 1704076200000,
      "side": "long",
      "strategy": "strat-a"
    },
    {
      "close_ms": 1704078900000,
      "coin": "ETH",
      "open_ms": 1704078300000,
      "side": "long",
      "strategy": "strat-a"
    },
    {
      "close_ms": 1704078300000,
      "coin": "SOL",
      "open_ms": 1704077400000,
      "side": "long",
      "strategy": "strat-a"
    },
    {
      "close_ms": 1704075900000,
      "coin": "ETH",
      "open_ms": 1704075000000,
      "side": "long",
      "strategy": "strat-b"
    }
  ]
}
```

## Blockers

```json
[
  {
    "evidence_sha256": "00754fd81c95176de62df5184948a22ec843c286cafaa14ea255f44c71a35400",
    "fact": "candle_price_basis",
    "status": "unconfirmed"
  },
  {
    "evidence_sha256": "59668da8877d2855e399c22d98183cd36d525e25575c11cbdbefd6265ab68f69",
    "fact": "cancel_time_source",
    "status": "unconfirmed"
  },
  {
    "fact": "production_capture",
    "reason": "production host, live account address, and venue network were not available",
    "status": "pending"
  }
]
```

## Fee rates on take-profit fills

These rates are evidence for issue 1726. This study changes no fee.

```json
[
  {
    "fee_rate": "0.00015",
    "order_ordinal": 2
  },
  {
    "fee_rate": "0.00015",
    "order_ordinal": 3
  },
  {
    "fee_rate": "0.00015",
    "order_ordinal": 10
  }
]
```
