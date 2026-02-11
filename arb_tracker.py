"""
Polymarket + Binance Arbitrage Opportunity Tracker

Compares Polymarket prediction market prices against real-time Binance spot data
to identify mispricings. Logs opportunities without executing trades.
"""

import json
import time
import os
import threading
import math
from datetime import datetime, timezone, timedelta
from typing import Dict, List, Optional
import logging

# Use ccxt for Binance (already available in trading bot)
import ccxt

from polymarket_client import PolymarketClient

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(name)s: %(message)s'
)
logger = logging.getLogger(__name__)

# Config
LOG_DIR = os.path.join(os.path.dirname(__file__), 'arb_logs')
MEMORY_FILE = os.path.join(os.path.dirname(__file__), 'arb_memories.json')
BINANCE_POLL_INTERVAL = 10    # seconds
POLYMARKET_POLL_INTERVAL = 60  # seconds
ARB_THRESHOLD_PCT = 2.0       # minimum spread % to log as opportunity
MAX_SPREAD_PCT = 50.0         # filter out data errors
MEMORY_SAVE_INTERVAL = 300    # save memories every 5 min


class ArbTracker:
    def __init__(self):
        # Use Kraken instead of Binance (Binance geo-blocks Hetzner IPs)
        self.exchange = ccxt.kraken()
        self.polymarket = PolymarketClient()
        self.running = False

        # Current state
        self.spot_prices: Dict[str, float] = {}
        self.poly_markets: List[Dict] = []

        # Memory
        self.memories = self._load_memories()
        self._last_memory_save = time.time()

        # Ensure log dir
        os.makedirs(LOG_DIR, exist_ok=True)

    def start(self):
        """Start the tracker"""
        self.running = True
        logger.info("Starting arbitrage tracker...")

        # Start threads
        binance_thread = threading.Thread(target=self._poll_binance, daemon=True)
        poly_thread = threading.Thread(target=self._poll_polymarket, daemon=True)
        scanner_thread = threading.Thread(target=self._scan_opportunities, daemon=True)

        binance_thread.start()
        poly_thread.start()
        scanner_thread.start()

        try:
            while self.running:
                time.sleep(1)
                # Periodic memory save
                if time.time() - self._last_memory_save > MEMORY_SAVE_INTERVAL:
                    self._save_memories()
                    self._last_memory_save = time.time()
        except KeyboardInterrupt:
            logger.info("Shutting down...")
            self.running = False
            self._save_memories()

    def _poll_binance(self):
        """Poll Kraken for spot prices (Binance geo-blocks this server)"""
        symbols = ['BTC/USDT', 'ETH/USDT', 'SOL/USD']
        while self.running:
            try:
                for symbol in symbols:
                    ticker = self.exchange.fetch_ticker(symbol)
                    short = symbol.split('/')[0]
                    self.spot_prices[short] = ticker['last']
                    logger.debug(f"Kraken {short}: ${ticker['last']:,.2f}")
            except Exception as e:
                logger.error(f"Kraken poll error: {e}")
            time.sleep(BINANCE_POLL_INTERVAL)

    def _poll_polymarket(self):
        """Poll Polymarket for prediction market data"""
        while self.running:
            try:
                events = self.polymarket.get_crypto_events()
                markets = self.polymarket.parse_markets(events)
                self.poly_markets = markets  # prices already embedded from gamma API
                logger.info(f"Polymarket: {len(markets)} priced markets, Spot: {self.spot_prices}")
            except Exception as e:
                logger.error(f"Polymarket poll error: {e}")
            time.sleep(POLYMARKET_POLL_INTERVAL)

    def _scan_opportunities(self):
        """Compare Polymarket implied probs against spot data to find arb"""
        # Wait for initial data
        time.sleep(15)

        while self.running:
            try:
                if not self.spot_prices or not self.poly_markets:
                    time.sleep(5)
                    continue

                for market in self.poly_markets:
                    opp = self._evaluate_opportunity(market)
                    if opp:
                        self._log_opportunity(opp)
                        self._update_memories(opp)

            except Exception as e:
                logger.error(f"Scanner error: {e}")

            time.sleep(BINANCE_POLL_INTERVAL)

    def _evaluate_opportunity(self, market: Dict) -> Optional[Dict]:
        """
        Evaluate if a Polymarket prediction is mispriced vs spot.

        For "Will BTC hit $150k by March 2026?" type markets:
        - Get current BTC spot price from Binance
        - Calculate a theoretical probability based on:
          - Distance from target (how far price needs to move)
          - Time remaining (more time = higher prob)
          - Historical volatility proxy
        - Compare theoretical prob vs Polymarket implied prob
        - If spread > threshold, log as opportunity
        """
        symbol = market.get('symbol')
        target_price = market.get('target_price')
        implied_prob = market.get('implied_prob')

        if not symbol or not target_price or implied_prob is None:
            return None

        spot = self.spot_prices.get(symbol)
        if not spot:
            return None

        # Calculate distance to target
        distance_pct = ((target_price - spot) / spot) * 100

        # Estimate time remaining (rough, based on deadline text)
        days_remaining = self._estimate_days_remaining(market.get('deadline', ''))

        # Calculate theoretical probability
        # Using a simplified model based on distance and time
        theoretical_prob = self._calc_theoretical_prob(
            spot, target_price, days_remaining, symbol
        )

        if theoretical_prob is None:
            return None

        # Calculate spread
        spread_pct = (theoretical_prob - implied_prob) * 100

        if abs(spread_pct) < ARB_THRESHOLD_PCT:
            return None
        if abs(spread_pct) > MAX_SPREAD_PCT:
            return None

        direction = "BUY_YES" if spread_pct > 0 else "BUY_NO"

        return {
            'timestamp': datetime.now(timezone.utc).isoformat(),
            'symbol': symbol,
            'spot_price': spot,
            'target_price': target_price,
            'distance_pct': round(distance_pct, 2),
            'days_remaining': days_remaining,
            'implied_prob': round(implied_prob, 4),
            'theoretical_prob': round(theoretical_prob, 4),
            'spread_pct': round(abs(spread_pct), 2),
            'direction': direction,
            'question': market.get('question', ''),
            'contract_id': market.get('yes_token', '')[:40],
            'event': market.get('event_title', ''),
        }

    def _calc_theoretical_prob(
        self, spot: float, target: float, days: int, symbol: str
    ) -> Optional[float]:
        """
        Simplified probability model.

        Uses log-normal assumption with annualized vol estimates:
        - BTC: ~60% annual vol
        - ETH: ~75% annual vol
        - SOL: ~90% annual vol
        """
        vol_map = {'BTC': 0.60, 'ETH': 0.75, 'SOL': 0.90}
        annual_vol = vol_map.get(symbol, 0.70)

        if days <= 0:
            # Already past deadline
            return 1.0 if spot >= target else 0.0

        # Time in years
        t = days / 365.0

        # Log-normal model: P(S_T > K) = N(d2)
        # d2 = (ln(S/K) + (r - 0.5*sigma^2)*T) / (sigma*sqrt(T))
        # Using r=0 (risk-neutral simplified)
        try:
            sigma_sqrt_t = annual_vol * math.sqrt(t)
            d2 = (math.log(spot / target) + (-0.5 * annual_vol**2) * t) / sigma_sqrt_t

            # Approximate normal CDF
            prob = self._norm_cdf(d2)
            return max(0.01, min(0.99, prob))
        except (ValueError, ZeroDivisionError):
            return None

    def _norm_cdf(self, x: float) -> float:
        """Approximate standard normal CDF"""
        return 0.5 * (1 + math.erf(x / math.sqrt(2)))

    def _estimate_days_remaining(self, deadline: Optional[str]) -> int:
        """Estimate days remaining from deadline text"""
        if not deadline:
            return 180  # default 6 months

        now = datetime.now(timezone.utc)
        deadline_lower = deadline.lower().strip()

        # Try parsing common formats
        month_map = {
            'january': 1, 'february': 2, 'march': 3, 'april': 4,
            'may': 5, 'june': 6, 'july': 7, 'august': 8,
            'september': 9, 'october': 10, 'november': 11, 'december': 12
        }

        for month_name, month_num in month_map.items():
            if month_name in deadline_lower:
                import re
                # Try to find day and year
                day_match = re.search(r'(\d{1,2})', deadline)
                year_match = re.search(r'(20\d{2})', deadline)
                day = int(day_match.group(1)) if day_match else 28
                year = int(year_match.group(1)) if year_match else now.year
                if year < now.year:
                    year = now.year + 1

                try:
                    target_date = datetime(year, month_num, min(day, 28), tzinfo=timezone.utc)
                    delta = (target_date - now).days
                    return max(1, delta)
                except ValueError:
                    pass

        # Check for just a year
        import re
        year_match = re.search(r'(20\d{2})', deadline or '')
        if year_match:
            year = int(year_match.group(1))
            target_date = datetime(year, 12, 31, tzinfo=timezone.utc)
            delta = (target_date - now).days
            return max(1, delta)

        return 180

    def _log_opportunity(self, opp: Dict):
        """Log opportunity to JSONL file"""
        date_str = datetime.now(timezone.utc).strftime('%Y-%m-%d')
        log_file = os.path.join(LOG_DIR, f'arb_{date_str}.jsonl')

        with open(log_file, 'a') as f:
            f.write(json.dumps(opp) + '\n')

        logger.info(
            f"🎯 ARB: {opp['symbol']} | {opp['direction']} | "
            f"spread={opp['spread_pct']:.1f}% | "
            f"implied={opp['implied_prob']:.1%} vs theo={opp['theoretical_prob']:.1%} | "
            f"{opp['question'][:60]}"
        )

    def _update_memories(self, opp: Dict):
        """Update memory patterns"""
        mem = self.memories
        mem['total_opportunities'] = mem.get('total_opportunities', 0) + 1
        mem['last_updated'] = opp['timestamp']

        # Track by symbol
        sym_stats = mem.setdefault('by_symbol', {})
        sym = sym_stats.setdefault(opp['symbol'], {
            'count': 0, 'total_spread': 0, 'avg_spread': 0,
            'max_spread': 0, 'directions': {'BUY_YES': 0, 'BUY_NO': 0}
        })
        sym['count'] += 1
        sym['total_spread'] += opp['spread_pct']
        sym['avg_spread'] = round(sym['total_spread'] / sym['count'], 2)
        sym['max_spread'] = max(sym['max_spread'], opp['spread_pct'])
        sym['directions'][opp['direction']] = sym['directions'].get(opp['direction'], 0) + 1

        # Track by hour
        hour = datetime.fromisoformat(opp['timestamp']).hour
        hourly = mem.setdefault('by_hour', {})
        hourly[str(hour)] = hourly.get(str(hour), 0) + 1

        # Track largest spreads
        biggest = mem.setdefault('largest_spreads', [])
        biggest.append({
            'spread_pct': opp['spread_pct'],
            'symbol': opp['symbol'],
            'direction': opp['direction'],
            'question': opp['question'][:80],
            'timestamp': opp['timestamp']
        })
        biggest.sort(key=lambda x: x['spread_pct'], reverse=True)
        mem['largest_spreads'] = biggest[:20]  # keep top 20

    def _load_memories(self) -> Dict:
        """Load memory file"""
        if os.path.exists(MEMORY_FILE):
            try:
                with open(MEMORY_FILE) as f:
                    return json.load(f)
            except Exception:
                pass
        return {'total_opportunities': 0, 'by_symbol': {}, 'by_hour': {}, 'largest_spreads': []}

    def _save_memories(self):
        """Save memory file"""
        try:
            with open(MEMORY_FILE, 'w') as f:
                json.dump(self.memories, f, indent=2)
            logger.info(f"Saved memories ({self.memories.get('total_opportunities', 0)} total opportunities)")
        except Exception as e:
            logger.error(f"Failed to save memories: {e}")


if __name__ == '__main__':
    tracker = ArbTracker()
    tracker.start()
