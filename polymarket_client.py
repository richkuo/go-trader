"""
Polymarket CLOB API Client
Handles REST connections to Polymarket's prediction markets.
Note: WebSocket functionality replaced with polling due to package availability.
"""

import asyncio
import json
import requests
from typing import Dict, List, Optional, Any
import logging
from datetime import datetime, timezone
import time

# Configure logging
logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


class PolymarketClient:
    """Client for interacting with Polymarket CLOB API"""
    
    def __init__(self, base_url: str = "https://clob.polymarket.com"):
        self.base_url = base_url
        self.session = requests.Session()
        self.session.headers.update({
            'User-Agent': 'PolymarketArbitrageTracker/1.0'
        })
        
    def __enter__(self):
        return self
    
    def __exit__(self, exc_type, exc_val, exc_tb):
        self.session.close()
    
    def get_markets(self, active: bool = True) -> List[Dict[str, Any]]:
        """Fetch all markets from Polymarket"""
        try:
            params = {"active": "true" if active else "false"}
            response = self.session.get(f"{self.base_url}/markets", params=params, timeout=10)
            
            if response.status_code == 200:
                data = response.json()
                return data if isinstance(data, list) else data.get('data', [])
            else:
                logger.error(f"Failed to fetch markets: {response.status_code}")
                return []
        except Exception as e:
            logger.error(f"Error fetching markets: {e}")
            return []
    
    def get_crypto_markets(self) -> List[Dict[str, Any]]:
        """Filter markets for crypto-related prediction markets"""
        all_markets = self.get_markets()
        crypto_keywords = [
            'bitcoin', 'btc', 'ethereum', 'eth', 'solana', 'sol', 'crypto', 
            'cryptocurrency', 'price', '$50000', '$100000', '$200', '$500',
            'coin', 'token', 'blockchain'
        ]
        
        crypto_markets = []
        for market in all_markets:
            market_text = (
                market.get('question', '') + ' ' + 
                market.get('description', '') + ' ' +
                market.get('market_slug', '')
            ).lower()
            
            if any(keyword in market_text for keyword in crypto_keywords):
                crypto_markets.append(market)
        
        return crypto_markets
    
    def get_market_orderbook(self, token_id: str) -> Dict[str, Any]:
        """Get orderbook for a specific market token"""
        try:
            response = self.session.get(f"{self.base_url}/book", params={"token_id": token_id}, timeout=10)
            
            if response.status_code == 200:
                return response.json()
            else:
                logger.error(f"Failed to fetch orderbook for {token_id}: {response.status_code}")
                return {}
        except Exception as e:
            logger.error(f"Error fetching orderbook for {token_id}: {e}")
            return {}
    
    def get_market_price(self, token_id: str) -> Optional[float]:
        """Get current market price for a token"""
        orderbook = self.get_market_orderbook(token_id)
        
        if not orderbook:
            return None
        
        # Get best bid and ask to calculate mid price
        bids = orderbook.get('bids', [])
        asks = orderbook.get('asks', [])
        
        if not bids or not asks:
            return None
        
        try:
            best_bid = float(bids[0]['price']) if bids else 0
            best_ask = float(asks[0]['price']) if asks else 1
            mid_price = (best_bid + best_ask) / 2
            return mid_price
        except (ValueError, KeyError, IndexError):
            return None
    
    def extract_crypto_price_target(self, market_data: Dict[str, Any]) -> Optional[Dict[str, Any]]:
        """Extract crypto price targets from market questions"""
        question = market_data.get('question', '').lower()
        
        # Look for price targets in questions
        price_patterns = [
            ('btc', 'bitcoin'),
            ('eth', 'ethereum'), 
            ('sol', 'solana')
        ]
        
        for symbol, name in price_patterns:
            if symbol in question or name in question:
                # Try to extract price target
                import re
                price_match = re.search(r'\$(\d{1,3}(?:,\d{3})*(?:\.\d{2})?)', question)
                if price_match:
                    price_str = price_match.group(1).replace(',', '')
                    try:
                        target_price = float(price_str)
                        return {
                            'symbol': symbol.upper(),
                            'target_price': target_price,
                            'market_id': market_data.get('condition_id'),
                            'question': market_data.get('question'),
                            'outcome_tokens': market_data.get('tokens', [])
                        }
                    except ValueError:
                        pass
        
        return None
    
    def get_market_updates(self, callback):
        """Get market updates and call callback for each one"""
        try:
            crypto_markets = self.get_crypto_markets()
            
            for market in crypto_markets:
                crypto_data = self.extract_crypto_price_target(market)
                if crypto_data:
                    # Get prices for each outcome token
                    for token in crypto_data['outcome_tokens']:
                        token_id = token.get('token_id')
                        if token_id:
                            price = self.get_market_price(token_id)
                            if price is not None:
                                market_update = {
                                    'timestamp': datetime.now(timezone.utc).isoformat(),
                                    'type': 'market_update',
                                    'symbol': crypto_data['symbol'],
                                    'target_price': crypto_data['target_price'],
                                    'market_price': price,
                                    'implied_prob': price,  # Price is probability in prediction markets
                                    'contract_id': token_id,
                                    'question': crypto_data['question'],
                                    'outcome': token.get('outcome', 'unknown')
                                }
                                callback(market_update)
                                    
        except Exception as e:
            logger.error(f"Market update error: {e}")
    
    def get_market_summary(self) -> List[Dict[str, Any]]:
        """Get a summary of all crypto-related markets"""
        crypto_markets = self.get_crypto_markets()
        summaries = []
        
        for market in crypto_markets:
            crypto_data = self.extract_crypto_price_target(market)
            if crypto_data:
                summary = {
                    'market_id': crypto_data['market_id'],
                    'question': crypto_data['question'],
                    'symbol': crypto_data['symbol'],
                    'target_price': crypto_data['target_price'],
                    'tokens': []
                }
                
                for token in crypto_data['outcome_tokens']:
                    token_id = token.get('token_id')
                    if token_id:
                        price = self.get_market_price(token_id)
                        summary['tokens'].append({
                            'token_id': token_id,
                            'outcome': token.get('outcome', 'unknown'),
                            'price': price
                        })
                
                summaries.append(summary)
        
        return summaries


# Test function
def test_polymarket_client():
    """Test the Polymarket client"""
    with PolymarketClient() as client:
        print("Testing Polymarket client...")
        
        # Test getting markets
        crypto_markets = client.get_crypto_markets()
        print(f"Found {len(crypto_markets)} crypto-related markets")
        
        for market in crypto_markets[:3]:  # Show first 3
            print(f"Market: {market.get('question', 'Unknown')}")
            crypto_data = client.extract_crypto_price_target(market)
            if crypto_data:
                print(f"  Symbol: {crypto_data['symbol']}")
                print(f"  Target Price: ${crypto_data['target_price']:,.2f}")
        
        # Test market summary
        summaries = client.get_market_summary()
        print(f"\nMarket summaries: {len(summaries)}")
        for summary in summaries[:2]:
            print(f"  {summary['question']}")
            print(f"  Target: {summary['symbol']} @ ${summary['target_price']:,.2f}")


if __name__ == "__main__":
    test_polymarket_client()