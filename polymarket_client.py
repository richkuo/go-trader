"""
Polymarket Client — Uses gamma-api.polymarket.com for market discovery
and clob.polymarket.com for orderbook/pricing data.
"""

import json
import requests
from typing import Dict, List, Optional, Any
import logging
import re
from datetime import datetime, timezone

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


class PolymarketClient:
    def __init__(self):
        self.gamma_url = "https://gamma-api.polymarket.com"
        self.clob_url = "https://clob.polymarket.com"
        self.session = requests.Session()
        self.session.headers.update({
            'User-Agent': 'ArbTracker/2.0'
        })
        self._market_cache = {}
        self._cache_ts = 0

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.session.close()

    def get_crypto_events(self) -> List[Dict[str, Any]]:
        """Fetch active crypto-related events from gamma API"""
        try:
            all_events = []
            for offset in range(0, 1000, 100):
                resp = self.session.get(
                    f"{self.gamma_url}/events",
                    params={"limit": 100, "active": "true", "closed": "false", "offset": offset},
                    timeout=15
                )
                if resp.status_code != 200:
                    break
                batch = resp.json()
                if not batch:
                    break
                all_events.extend(batch)

            # Filter for crypto price-related events
            crypto_keywords = [
                'bitcoin', 'btc', 'ethereum', 'eth', 'solana', 'sol',
                'crypto', 'token launch', 'megaeth'
            ]
            # Specifically target price prediction markets
            price_keywords = [
                'hit $', 'above $', 'below $', 'reach $', 'price',
                '$150k', '$200k', '$100k', '$1m', 'valuable than',
                'hold $', 'buys bitcoin', 'reserve', 'unban bitcoin'
            ]

            crypto_events = []
            for event in all_events:
                title = event.get('title', '').lower()
                questions = ' '.join(
                    m.get('question', '') for m in event.get('markets', [])
                ).lower()
                combined = title + ' ' + questions

                has_crypto = any(k in combined for k in crypto_keywords)
                has_price = any(k in combined for k in price_keywords)

                if has_crypto and has_price:
                    crypto_events.append(event)

            logger.info(f"Found {len(crypto_events)} crypto price events from {len(all_events)} total")
            return crypto_events

        except Exception as e:
            logger.error(f"Error fetching events: {e}")
            return []

    def parse_markets(self, events: List[Dict]) -> List[Dict[str, Any]]:
        """Parse events into structured market data with prices from gamma API"""
        markets = []
        for event in events:
            for market in event.get('markets', []):
                # Skip closed markets
                if market.get('closed'):
                    continue

                question = market.get('question', '')
                clob_tokens = market.get('clobTokenIds', [])

                # Get prices directly from gamma API (outcomePrices)
                try:
                    prices = json.loads(market.get('outcomePrices', '[]'))
                    yes_price = float(prices[0]) if prices else None
                    no_price = float(prices[1]) if len(prices) > 1 else None
                except (json.JSONDecodeError, ValueError, IndexError):
                    yes_price = None
                    no_price = None

                if not yes_price or yes_price == 0:
                    continue

                # Determine crypto symbol
                q_lower = question.lower()
                symbol = None
                if any(k in q_lower for k in ['bitcoin', 'btc']):
                    symbol = 'BTC'
                elif any(k in q_lower for k in ['ethereum', 'eth']):
                    symbol = 'ETH'
                elif any(k in q_lower for k in ['solana', 'sol']):
                    symbol = 'SOL'

                if not symbol:
                    continue

                # Extract price target
                target_price = self._extract_price(question)
                if not target_price:
                    continue

                # Extract deadline
                deadline = self._extract_deadline(question)

                markets.append({
                    'event_title': event.get('title', ''),
                    'question': question,
                    'symbol': symbol,
                    'target_price': target_price,
                    'deadline': deadline,
                    'yes_token': clob_tokens[0] if len(clob_tokens) > 0 else None,
                    'no_token': clob_tokens[1] if len(clob_tokens) > 1 else None,
                    'condition_id': market.get('conditionId', ''),
                    'yes_price': yes_price,
                    'no_price': no_price,
                    'implied_prob': yes_price,
                })

        logger.info(f"Parsed {len(markets)} tradeable crypto price markets")
        return markets

    def get_token_price(self, token_id: str) -> Optional[float]:
        """Get mid price for a token from CLOB orderbook"""
        try:
            resp = self.session.get(
                f"{self.clob_url}/book",
                params={"token_id": token_id},
                timeout=10
            )
            if resp.status_code != 200:
                return None

            book = resp.json()
            bids = book.get('bids', [])
            asks = book.get('asks', [])

            if bids and asks:
                best_bid = float(bids[0]['price'])
                best_ask = float(asks[0]['price'])
                return (best_bid + best_ask) / 2
            elif bids:
                return float(bids[0]['price'])
            elif asks:
                return float(asks[0]['price'])
            return None

        except Exception as e:
            logger.debug(f"Orderbook fetch failed for {token_id[:20]}...: {e}")
            return None

    def get_market_prices(self, markets: List[Dict]) -> List[Dict]:
        """Enrich markets with current YES/NO prices"""
        enriched = []
        for market in markets:
            yes_price = None
            no_price = None

            if market.get('yes_token'):
                yes_price = self.get_token_price(market['yes_token'])
            if market.get('no_token'):
                no_price = self.get_token_price(market['no_token'])

            market['yes_price'] = yes_price
            market['no_price'] = no_price
            market['implied_prob'] = yes_price  # YES price = implied probability

            if yes_price is not None:
                enriched.append(market)

        logger.info(f"Got prices for {len(enriched)}/{len(markets)} markets")
        return enriched

    def _extract_price(self, question: str) -> Optional[float]:
        """Extract dollar price target from question text"""
        # Match patterns like $150k, $1m, $100,000, $1b
        patterns = [
            r'\$(\d+(?:\.\d+)?)\s*[kK]',      # $150k
            r'\$(\d+(?:\.\d+)?)\s*[mM]',       # $1m
            r'\$(\d+(?:\.\d+)?)\s*[bB]',       # $1b
            r'\$(\d{1,3}(?:,\d{3})+)',          # $100,000
            r'\$(\d+(?:\.\d+)?)',               # $100
        ]

        for i, pattern in enumerate(patterns):
            match = re.search(pattern, question)
            if match:
                val = match.group(1).replace(',', '')
                num = float(val)
                if i == 0:  # k
                    return num * 1000
                elif i == 1:  # m
                    return num * 1_000_000
                elif i == 2:  # b
                    return num * 1_000_000_000
                return num
        return None

    def _extract_deadline(self, question: str) -> Optional[str]:
        """Extract deadline from question"""
        patterns = [
            r'by ((?:January|February|March|April|May|June|July|August|September|October|November|December)\s+\d{1,2}(?:,?\s*\d{4})?)',
            r'in (\d{4})',
            r'before (\d{4})',
        ]
        for pattern in patterns:
            match = re.search(pattern, question, re.IGNORECASE)
            if match:
                return match.group(1)
        return None


def test_client():
    with PolymarketClient() as client:
        print("Fetching crypto events...")
        events = client.get_crypto_events()
        print(f"\nFound {len(events)} crypto price events")

        markets = client.parse_markets(events)
        print(f"Parsed {len(markets)} markets")

        print("\nFetching prices (first 5)...")
        priced = client.get_market_prices(markets[:5])
        for m in priced:
            print(f"\n  Q: {m['question']}")
            print(f"  Symbol: {m['symbol']}, Target: ${m['target_price']:,.0f}" if m['target_price'] else f"  Symbol: {m['symbol']}")
            print(f"  YES: {m['yes_price']:.4f}, Implied prob: {m['implied_prob']:.1%}" if m['yes_price'] else "  No price")


if __name__ == "__main__":
    test_client()
