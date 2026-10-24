# OKX public order book source for research pairs (#1871)

This note documents the OKX depth source that a research pair's OKX member reads (source label `public_snapshot`, source contract `okx_public_books_snapshot_v1`).
SKILL.md section Pair quote collection and envelopes owns the full envelope contract; this page covers only the OKX source.

## Endpoint

- `GET /api/v5/market/books` on the mainnet host `https://www.okx.com`, through the ccxt public client (`publicGetMarketBooks`, present in the locked ccxt 4.5.76 at `ccxt/abstract/okx.py`).
- Request parameters: `instId` is the member's native instrument (`<UNDERLYING>-USDT-SWAP`; #1869 refuses any other OKX instrument) and `sz` is the decimal text of `book_need.depth` (1 to 20).
- `OKXExchangeAdapter(public_only=True).public_order_book(inst_id, depth)` sends no key, signs nothing and loads no markets, so ccxt never converts the book to floats.
  The adapter's `public_host()` must be `https://www.okx.com`; `OKX_SANDBOX` must be empty (the reader refuses a nonempty value and Go passes an empty override).

## Response and payload

- A success needs `code` `"0"`, a `msg` string and exactly one book in `data`, with `asks`, `bids` and `ts`.
- Each level is four text fields `[px, sz, liquidated, orders]`: price, size, a deprecated liquidated-orders field and the order count.
  The reader keeps the raw text; the verifier requires `px` and `sz` to be plain positive decimals of at most 32 characters and 18 fractional digits, the third field to be a plain decimal and the order count a plain integer.
- The frozen payload is the #1869 canonical bytes of `{"request":{"instId","sz"},"response":{"code","data":[{"asks","bids","ts"}],"msg"}}`.
  Binding the request inside the payload lets a verifier check the instrument and the requested depth, because the response does not repeat the instrument.

## Units and currencies

- `sz` is native contracts.
  The envelope builder computes base quantity once as contracts times the capability record's `contract_size` (`ctVal` in the #1869 derivation table), with exact decimal arithmetic.
- Prices stay in the native price currency, USDT for every admitted OKX member; no stablecoin is treated as USD, and conversions come from the frozen evidence schedule.

## Time and provenance

- `ts` is the venue time of the book in milliseconds; it must be at most 250 ms after the receipt and at most 5000 ms before it.
- `first_receipt_ms` is the reader clock right after the response; Go checks that the reader's request and receipt times fall inside its own launch window.
- `provenance` is `okx_public_books_snapshot_v1@https://www.okx.com`.

## Coverage

- One complete depth snapshot at one point: `from_ms = through_ms = first_receipt_ms`, `required_depth = book_need.depth`, `required_window_ms = 0`, `required_capacity = 1`, prefix complete, status `complete`, no gaps.
- It never claims websocket or continuous history; a consumer that needs a history window must refuse it.
- A book with fewer levels than the required depth on a side that must meet depth, a crossed or locked book, an unknown instrument (`code` other than `"0"`) or a stale receipt holds the collection slot; nothing substitutes a midpoint, last price or Hyperliquid data.
