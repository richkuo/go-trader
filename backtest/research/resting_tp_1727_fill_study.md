# Resting take-profit fill study

Read-only measurement for issue 1727. The capture tool and this study stay outside the scheduler. The study reads a frozen ledger export, a capture bundle, and a hash-checked candle manifest. It submits no order and writes no config or state.

Placement time, terminal time, requested size and limit price come only from venue order records. Export timestamps only attribute an order to a position. A missing venue field is `lifetime_unknown`. Tier prices, manual additions and replacement orders are not rebuilt from current configuration.

A bar that overlaps placement, cancellation or replacement is unknown. Another order's fill does not make that bar unknown, and neither does another order's cancel or placement at or after this order's first fill.

Rules predict only from scored bars: bars wholly inside the resting interval before the first fill (an order with fills) or before the terminal status (other orders). The bar that holds the first fill or the terminal status is never a prediction input, because its extreme can come after the order stopped resting. An order with fills and no scored bar is `terminal_bar_only`; it is counted and kept out of the rule counts. On an unfilled order a prediction is a false fill, else an agreement. On an order with fills a prediction on a scored bar is an early fill, and also a quantity error when the venue filled only part. A missed fill is counted only as a terminal-bar upper-bound proof: no bar from placement through the fill bar reaches the rule threshold. Any other order with fills is `fill_bar_unresolved`. An unconfirmed candle basis or cancel-time source is a blocker. Raising k does not repair unknown placement timing.

The frozen manifest interval is 5m because the manifest verifier has no 1m interval. Live capture asks for candles at the interval passed to the capture tool. A capture start that is not on that interval's boundary is refused before any info request. The venue keeps only the most recent 5000 candles of that interval. Each manifest bar must equal the snapshot bar at the same open, including high, low and close, and each own fill must lie inside its bar. Those are consistency checks only, because both sides are venue candles. The candle basis is confirmed only by independent public trade evidence: a basis probe of polled `recentTrades` that covers a full bar without a gap (consecutive polls share a trade id), with every trade inside the probe candle range and the trade maximum and minimum equal to the candle high and low, on every manifest coin. A capture without a probe stays unconfirmed with `no_independent_trade_evidence`.

## Candle basis evidence

```json
{
  "cancel_time_source": "unconfirmed",
  "candle_price_basis": "unconfirmed",
  "candle_price_basis_evidence": {
    "contradictions": 0,
    "covered_bars": 0,
    "evidence_sha256": "df79bd1c6f14c5f359b99da196150cfd73dd6b25b84dcbc16c450de939aba8db",
    "reasons": [
      "interval_mismatch",
      "no_independent_trade_evidence"
    ]
  }
}
```

## Rule counts

```json
{
  "rule_count_basis": {
    "agreements": "scored_bars",
    "early_fills": "scored_bars",
    "false_fills": "scored_bars",
    "fill_bar_unresolved": "terminal_bar",
    "missed_fills": "terminal_bar_upper_bound",
    "quantity_errors": "scored_bars"
  },
  "rules": {
    "close": {
      "long": {
        "agreements": 3,
        "bps_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        },
        "early_fills": 1,
        "false_fills": 0,
        "fill_bar_unresolved": 1,
        "missed_fills": 1,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        }
      },
      "short": {
        "agreements": 0,
        "bps_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        },
        "early_fills": 0,
        "false_fills": 0,
        "fill_bar_unresolved": 0,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        }
      }
    },
    "touch": {
      "long": {
        "agreements": 1,
        "bps_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        },
        "early_fills": 1,
        "false_fills": 2,
        "fill_bar_unresolved": 2,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        }
      },
      "short": {
        "agreements": 0,
        "bps_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        },
        "early_fills": 0,
        "false_fills": 0,
        "fill_bar_unresolved": 0,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        }
      }
    },
    "trade_through_1": {
      "long": {
        "agreements": 2,
        "bps_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        },
        "early_fills": 1,
        "false_fills": 1,
        "fill_bar_unresolved": 2,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        }
      },
      "short": {
        "agreements": 0,
        "bps_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        },
        "early_fills": 0,
        "false_fills": 0,
        "fill_bar_unresolved": 0,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        }
      }
    },
    "trade_through_2": {
      "long": {
        "agreements": 2,
        "bps_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        },
        "early_fills": 1,
        "false_fills": 1,
        "fill_bar_unresolved": 2,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        }
      },
      "short": {
        "agreements": 0,
        "bps_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        },
        "early_fills": 0,
        "false_fills": 0,
        "fill_bar_unresolved": 0,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        }
      }
    },
    "trade_through_3": {
      "long": {
        "agreements": 2,
        "bps_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        },
        "early_fills": 1,
        "false_fills": 1,
        "fill_bar_unresolved": 2,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        }
      },
      "short": {
        "agreements": 0,
        "bps_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        },
        "early_fills": 0,
        "false_fills": 0,
        "fill_bar_unresolved": 0,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        }
      }
    },
    "trade_through_4": {
      "long": {
        "agreements": 3,
        "bps_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        },
        "early_fills": 1,
        "false_fills": 0,
        "fill_bar_unresolved": 1,
        "missed_fills": 1,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        }
      },
      "short": {
        "agreements": 0,
        "bps_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        },
        "early_fills": 0,
        "false_fills": 0,
        "fill_bar_unresolved": 0,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        }
      }
    },
    "trade_through_5": {
      "long": {
        "agreements": 3,
        "bps_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        },
        "early_fills": 1,
        "false_fills": 0,
        "fill_bar_unresolved": 0,
        "missed_fills": 2,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": 4,
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": 1,
          "5-9": 1
        }
      },
      "short": {
        "agreements": 0,
        "bps_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        },
        "early_fills": 0,
        "false_fills": 0,
        "fill_bar_unresolved": 0,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {
          "0": "unmeasured",
          "1": "unmeasured",
          "10+": "unmeasured",
          "2-4": "unmeasured",
          "5-9": "unmeasured"
        }
      }
    }
  }
}
```

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
  "orders_per_prediction_status": {
    "scored": 6
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
    "evidence_sha256": "df79bd1c6f14c5f359b99da196150cfd73dd6b25b84dcbc16c450de939aba8db",
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
