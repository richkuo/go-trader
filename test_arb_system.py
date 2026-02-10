#!/usr/bin/env python3
"""
Test script for the arbitrage tracking system
"""

import os
import sys
from pathlib import Path
import ccxt

# Add current directory to path
sys.path.insert(0, str(Path(__file__).parent))

from polymarket_client import PolymarketClient


def test_binance_connection():
    """Test Binance API connectivity"""
    print("Testing Binance connection...")
    try:
        exchange = ccxt.binanceus({
            'enableRateLimit': True,
            'sandbox': False,
        })
        
        btc_ticker = exchange.fetch_ticker('BTC/USDT')
        sol_ticker = exchange.fetch_ticker('SOL/USDT')
        
        print(f"✅ Binance connection successful!")
        print(f"   BTC/USDT: ${btc_ticker['last']:,.2f}")
        print(f"   SOL/USDT: ${sol_ticker['last']:,.2f}")
        return True
        
    except Exception as e:
        print(f"❌ Binance connection failed: {e}")
        return False


def test_polymarket_connection():
    """Test Polymarket API connectivity"""
    print("\nTesting Polymarket connection...")
    try:
        with PolymarketClient() as client:
            crypto_markets = client.get_crypto_markets()
            print(f"✅ Polymarket connection successful!")
            print(f"   Found {len(crypto_markets)} crypto-related markets")
            
            # Show a few examples
            for i, market in enumerate(crypto_markets[:3]):
                question = market.get('question', 'Unknown')
                print(f"   {i+1}. {question}")
                
            return True
            
    except Exception as e:
        print(f"❌ Polymarket connection failed: {e}")
        return False


def test_price_parsing():
    """Test crypto price target extraction"""
    print("\nTesting price parsing logic...")
    
    with PolymarketClient() as client:
        # Sample market data for testing
        test_market = {
            'question': 'Will Bitcoin (BTC) reach $100,000 by end of 2024?',
            'condition_id': 'test123',
            'tokens': [
                {'token_id': 'test_token_yes', 'outcome': 'Yes'},
                {'token_id': 'test_token_no', 'outcome': 'No'}
            ]
        }
        
        crypto_data = client.extract_crypto_price_target(test_market)
        
        if crypto_data:
            print("✅ Price parsing successful!")
            print(f"   Symbol: {crypto_data['symbol']}")
            print(f"   Target Price: ${crypto_data['target_price']:,.2f}")
            print(f"   Question: {crypto_data['question']}")
            return True
        else:
            print("❌ Price parsing failed - no crypto data extracted")
            return False


def main():
    """Run all tests"""
    print("🚀 Testing Arbitrage Tracking System")
    print("=" * 50)
    
    results = []
    results.append(test_binance_connection())
    results.append(test_polymarket_connection())
    results.append(test_price_parsing())
    
    print("\n" + "=" * 50)
    print("📊 Test Summary:")
    print(f"✅ Tests passed: {sum(results)}")
    print(f"❌ Tests failed: {len(results) - sum(results)}")
    
    if all(results):
        print("\n🎉 All systems operational! Ready to track arbitrage opportunities.")
    else:
        print("\n⚠️  Some issues detected. Check the error messages above.")
        print("Note: Polymarket API endpoints may have changed or require authentication.")
    
    print("\n📝 Next steps:")
    print("1. Run `python3 arb_tracker.py` to start tracking")
    print("2. Run `python3 arb_analyzer.py` to analyze logged opportunities")
    print("3. Check ARBITRAGE.md for detailed documentation")


if __name__ == "__main__":
    main()