"""
Polymarket + Binance Arbitrage Opportunity Tracker
Main tracker that connects to Binance API and Polymarket to identify arbitrage opportunities.
Note: Uses polling instead of WebSockets due to package availability constraints.
"""

import time
import json
import requests
import threading
from datetime import datetime, timezone
import logging
from typing import Dict, List, Optional, Any
from pathlib import Path
import os
import ccxt

from polymarket_client import PolymarketClient

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)


class ArbitrageTracker:
    """Main arbitrage tracker for Polymarket vs Binance price differences"""
    
    def __init__(self, log_dir: str = "arb_logs", memory_file: str = "arb_memories.json"):
        self.log_dir = Path(log_dir)
        self.memory_file = Path(memory_file)
        self.log_dir.mkdir(exist_ok=True)
        
        # Price tracking
        self.binance_prices = {'BTC': 0.0, 'SOL': 0.0}
        self.polymarket_data = {}
        self.opportunities = []
        
        # Arbitrage thresholds
        self.min_spread_pct = 2.0  # Minimum 2% spread to consider
        self.max_spread_pct = 50.0  # Maximum 50% spread (likely data error)
        
        # Memory for pattern tracking
        self.memory = self.load_memory()
        
        # Clients
        self.polymarket_client = PolymarketClient()
        self.binance_exchange = ccxt.binanceus({
            'enableRateLimit': True,
            'sandbox': False,
        })
        
        # Threading control
        self.running = False
        self.threads = []
        
    def load_memory(self) -> Dict[str, Any]:
        """Load memory file for pattern accumulation"""
        try:
            if self.memory_file.exists():
                with open(self.memory_file, 'r') as f:
                    return json.load(f)
        except Exception as e:
            logger.error(f"Error loading memory: {e}")
        
        return {
            'total_opportunities': 0,
            'opportunities_by_symbol': {},
            'avg_spreads': {},
            'largest_spreads': {},
            'patterns': {
                'time_of_day': {},
                'day_of_week': {},
                'market_conditions': {}
            },
            'last_updated': datetime.now(timezone.utc).isoformat()
        }
    
    def save_memory(self):
        """Save current memory state"""
        try:
            self.memory['last_updated'] = datetime.now(timezone.utc).isoformat()
            with open(self.memory_file, 'w') as f:
                json.dump(self.memory, f, indent=2)
        except Exception as e:
            logger.error(f"Error saving memory: {e}")
    
    def get_log_filename(self) -> str:
        """Generate log filename based on current date"""
        today = datetime.now(timezone.utc).strftime('%Y-%m-%d')
        return str(self.log_dir / f"arb_opportunities_{today}.jsonl")
    
    def poll_binance_prices(self):
        """Poll Binance for BTC and SOL prices"""
        logger.info("Starting Binance price polling...")
        
        while self.running:
            try:
                # Fetch current prices for BTC and SOL
                btc_ticker = self.binance_exchange.fetch_ticker('BTC/USDT')
                sol_ticker = self.binance_exchange.fetch_ticker('SOL/USDT')
                
                self.binance_prices['BTC'] = float(btc_ticker['last'])
                self.binance_prices['SOL'] = float(sol_ticker['last'])
                
                logger.debug(f"BTC price updated: ${self.binance_prices['BTC']:,.2f}")
                logger.debug(f"SOL price updated: ${self.binance_prices['SOL']:.2f}")
                
                # Check for arbitrage opportunities
                self.check_arbitrage_opportunities()
                
                time.sleep(10)  # Poll every 10 seconds
                    
            except Exception as e:
                logger.error(f"Binance polling error: {e}")
                time.sleep(30)  # Wait longer on error
    
    def process_polymarket_message(self, data: Dict[str, Any]):
        """Process incoming Polymarket market data"""
        try:
            symbol = data.get('symbol')
            if symbol in ['BTC', 'SOL']:
                contract_id = data.get('contract_id')
                self.polymarket_data[contract_id] = data
                logger.debug(f"Polymarket data updated for {symbol}: {data}")
                
                # Check for arbitrage opportunities
                self.check_arbitrage_opportunities()
                
        except Exception as e:
            logger.error(f"Error processing Polymarket message: {e}")
    
    def poll_polymarket_data(self):
        """Poll Polymarket for market data"""
        logger.info("Starting Polymarket data polling...")
        
        while self.running:
            try:
                # Get market updates
                self.polymarket_client.get_market_updates(
                    callback=self.process_polymarket_message
                )
                time.sleep(30)  # Wait 30 seconds before next poll
            except Exception as e:
                logger.error(f"Polymarket polling error: {e}")
                time.sleep(60)  # Wait longer on error
    
    def calculate_implied_probability(self, target_price: float, current_price: float, outcome: str) -> float:
        """Calculate implied probability from market price and target"""
        if outcome.lower() in ['yes', 'above', 'higher']:
            # Probability that price will be above target
            return 1.0 if current_price > target_price else current_price / target_price
        else:
            # Probability that price will be below target
            return 1.0 if current_price < target_price else target_price / current_price
    
    def check_arbitrage_opportunities(self):
        """Check for arbitrage opportunities between Binance and Polymarket"""
        try:
            for contract_id, poly_data in self.polymarket_data.items():
                symbol = poly_data.get('symbol')
                if symbol not in self.binance_prices:
                    continue
                
                binance_price = self.binance_prices[symbol]
                target_price = poly_data.get('target_price', 0)
                market_price = poly_data.get('market_price', 0)
                outcome = poly_data.get('outcome', '')
                
                if binance_price == 0 or target_price == 0 or market_price == 0:
                    continue
                
                # Calculate theoretical probability based on current price vs target
                theoretical_prob = self.calculate_implied_probability(target_price, binance_price, outcome)
                implied_prob = market_price  # Market price is the implied probability
                
                # Calculate spread
                spread_pct = abs(theoretical_prob - implied_prob) * 100
                
                # Determine direction
                if theoretical_prob > implied_prob:
                    direction = "market_undervalued"  # Market price too low
                else:
                    direction = "market_overvalued"  # Market price too high
                
                # Check if this is a significant arbitrage opportunity
                if self.min_spread_pct <= spread_pct <= self.max_spread_pct:
                    opportunity = {
                        'timestamp': datetime.now(timezone.utc).isoformat(),
                        'symbol': symbol,
                        'source_price': binance_price,
                        'target_price': target_price,
                        'market_price': market_price,
                        'implied_prob': implied_prob,
                        'theoretical_prob': theoretical_prob,
                        'spread_pct': spread_pct,
                        'direction': direction,
                        'contract_id': contract_id,
                        'question': poly_data.get('question', ''),
                        'outcome': outcome
                    }
                    
                    self.log_opportunity(opportunity)
                    self.update_memory(opportunity)
                    logger.info(f"Arbitrage opportunity: {symbol} {spread_pct:.2f}% spread - {direction}")
                    
        except Exception as e:
            logger.error(f"Error checking arbitrage opportunities: {e}")
    
    def log_opportunity(self, opportunity: Dict[str, Any]):
        """Log arbitrage opportunity to file"""
        try:
            log_file = self.get_log_filename()
            with open(log_file, 'a') as f:
                f.write(json.dumps(opportunity) + '\n')
            logger.debug(f"Logged opportunity to {log_file}")
        except Exception as e:
            logger.error(f"Error logging opportunity: {e}")
    
    def update_memory(self, opportunity: Dict[str, Any]):
        """Update memory with new opportunity patterns"""
        try:
            symbol = opportunity['symbol']
            spread = opportunity['spread_pct']
            timestamp = datetime.fromisoformat(opportunity['timestamp'].replace('Z', '+00:00'))
            
            # Update counts
            self.memory['total_opportunities'] += 1
            if symbol not in self.memory['opportunities_by_symbol']:
                self.memory['opportunities_by_symbol'][symbol] = 0
            self.memory['opportunities_by_symbol'][symbol] += 1
            
            # Update average spreads
            if symbol not in self.memory['avg_spreads']:
                self.memory['avg_spreads'][symbol] = []
            self.memory['avg_spreads'][symbol].append(spread)
            
            # Keep only last 100 spreads for rolling average
            if len(self.memory['avg_spreads'][symbol]) > 100:
                self.memory['avg_spreads'][symbol] = self.memory['avg_spreads'][symbol][-100:]
            
            # Update largest spreads
            if symbol not in self.memory['largest_spreads']:
                self.memory['largest_spreads'][symbol] = 0
            self.memory['largest_spreads'][symbol] = max(
                self.memory['largest_spreads'][symbol], spread
            )
            
            # Update time patterns
            hour = timestamp.hour
            day_of_week = timestamp.weekday()
            
            if str(hour) not in self.memory['patterns']['time_of_day']:
                self.memory['patterns']['time_of_day'][str(hour)] = 0
            self.memory['patterns']['time_of_day'][str(hour)] += 1
            
            if str(day_of_week) not in self.memory['patterns']['day_of_week']:
                self.memory['patterns']['day_of_week'][str(day_of_week)] = 0
            self.memory['patterns']['day_of_week'][str(day_of_week)] += 1
            
            # Save memory every 10 opportunities
            if self.memory['total_opportunities'] % 10 == 0:
                self.save_memory()
                
        except Exception as e:
            logger.error(f"Error updating memory: {e}")
    
    def start_tracking(self):
        """Start the arbitrage tracking system"""
        logger.info("Starting arbitrage tracking system...")
        
        try:
            self.running = True
            
            # Start Binance price polling thread
            binance_thread = threading.Thread(target=self.poll_binance_prices, daemon=True)
            binance_thread.start()
            self.threads.append(binance_thread)
            
            # Start Polymarket polling thread  
            polymarket_thread = threading.Thread(target=self.poll_polymarket_data, daemon=True)
            polymarket_thread.start()
            self.threads.append(polymarket_thread)
            
            # Start periodic memory save thread
            memory_thread = threading.Thread(target=self.periodic_memory_save, daemon=True)
            memory_thread.start()
            self.threads.append(memory_thread)
            
            # Keep main thread alive
            try:
                while self.running:
                    time.sleep(1)
                    # Check if all threads are still alive
                    for thread in self.threads:
                        if not thread.is_alive():
                            logger.warning(f"Thread {thread.name} died, restarting tracking...")
                            self.stop_tracking()
                            self.start_tracking()
                            return
            except KeyboardInterrupt:
                logger.info("Shutting down arbitrage tracker...")
                self.stop_tracking()
                
        except Exception as e:
            logger.error(f"Error in tracking system: {e}")
            self.stop_tracking()
        finally:
            logger.info("Arbitrage tracker stopped")
    
    def stop_tracking(self):
        """Stop the tracking system"""
        logger.info("Stopping tracking system...")
        self.running = False
        
        # Save final memory state
        self.save_memory()
        
        # Close clients
        if self.polymarket_client:
            self.polymarket_client.session.close()
    
    def periodic_memory_save(self):
        """Periodically save memory to prevent data loss"""
        while self.running:
            time.sleep(300)  # Save every 5 minutes
            self.save_memory()
    
    def print_stats(self):
        """Print current tracking statistics"""
        print("\n=== Arbitrage Tracker Statistics ===")
        print(f"Total opportunities found: {self.memory['total_opportunities']}")
        print(f"Opportunities by symbol: {self.memory['opportunities_by_symbol']}")
        
        for symbol, spreads in self.memory['avg_spreads'].items():
            if spreads:
                avg_spread = sum(spreads) / len(spreads)
                print(f"Average {symbol} spread: {avg_spread:.2f}%")
        
        print(f"Largest spreads: {self.memory['largest_spreads']}")
        print(f"Current Binance prices: {self.binance_prices}")
        print(f"Active Polymarket contracts: {len(self.polymarket_data)}")
        print("=" * 40)


def main():
    """Main entry point for the arbitrage tracker"""
    # Change to the correct directory
    script_dir = Path(__file__).parent
    os.chdir(script_dir)
    
    tracker = ArbitrageTracker()
    
    try:
        # Print initial stats
        tracker.print_stats()
        
        # Start tracking
        tracker.start_tracking()
        
    except KeyboardInterrupt:
        print("\nShutting down...")
        tracker.stop_tracking()
        tracker.print_stats()


if __name__ == "__main__":
    main()