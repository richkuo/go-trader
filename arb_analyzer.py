"""
Arbitrage Opportunity Analyzer
Analyzes logged arbitrage opportunities and identifies patterns over time.
"""

import json
import pandas as pd
from datetime import datetime, timezone, timedelta
from pathlib import Path
from typing import Dict, List, Any, Optional
from collections import defaultdict, Counter
import logging

# Configure logging
logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


class ArbitrageAnalyzer:
    """Analyzer for arbitrage opportunities and pattern detection"""
    
    def __init__(self, log_dir: str = "arb_logs", memory_file: str = "arb_memories.json"):
        self.log_dir = Path(log_dir)
        self.memory_file = Path(memory_file)
        self.opportunities_df = None
        self.memory_data = None
        
    def load_opportunities(self, days_back: int = 30) -> pd.DataFrame:
        """Load arbitrage opportunities from log files"""
        opportunities = []
        end_date = datetime.now(timezone.utc)
        start_date = end_date - timedelta(days=days_back)
        
        # Iterate through log files
        for log_file in self.log_dir.glob("arb_opportunities_*.jsonl"):
            try:
                # Extract date from filename
                date_str = log_file.stem.split('_')[-1]
                file_date = datetime.strptime(date_str, '%Y-%m-%d').replace(tzinfo=timezone.utc)
                
                # Skip if outside date range
                if file_date < start_date:
                    continue
                
                # Read opportunities from file
                with open(log_file, 'r') as f:
                    for line in f:
                        try:
                            opp = json.loads(line.strip())
                            opportunities.append(opp)
                        except json.JSONDecodeError as e:
                            logger.warning(f"Invalid JSON line in {log_file}: {e}")
                            
            except Exception as e:
                logger.error(f"Error reading {log_file}: {e}")
        
        if opportunities:
            df = pd.DataFrame(opportunities)
            df['timestamp'] = pd.to_datetime(df['timestamp'])
            df = df.sort_values('timestamp')
            logger.info(f"Loaded {len(df)} opportunities from {len(df)} records")
        else:
            df = pd.DataFrame()
            logger.warning("No opportunities loaded")
        
        self.opportunities_df = df
        return df
    
    def load_memory(self) -> Dict[str, Any]:
        """Load memory data"""
        try:
            if self.memory_file.exists():
                with open(self.memory_file, 'r') as f:
                    self.memory_data = json.load(f)
                    return self.memory_data
        except Exception as e:
            logger.error(f"Error loading memory: {e}")
        
        return {}
    
    def analyze_spread_patterns(self) -> Dict[str, Any]:
        """Analyze spread size patterns"""
        if self.opportunities_df is None or self.opportunities_df.empty:
            return {}
        
        df = self.opportunities_df
        analysis = {
            'overall_stats': {
                'total_opportunities': len(df),
                'avg_spread': df['spread_pct'].mean(),
                'median_spread': df['spread_pct'].median(),
                'max_spread': df['spread_pct'].max(),
                'min_spread': df['spread_pct'].min(),
                'std_spread': df['spread_pct'].std()
            },
            'by_symbol': {},
            'spread_distribution': {}
        }
        
        # Analyze by symbol
        for symbol in df['symbol'].unique():
            symbol_df = df[df['symbol'] == symbol]
            analysis['by_symbol'][symbol] = {
                'count': len(symbol_df),
                'avg_spread': symbol_df['spread_pct'].mean(),
                'median_spread': symbol_df['spread_pct'].median(),
                'max_spread': symbol_df['spread_pct'].max(),
                'direction_counts': symbol_df['direction'].value_counts().to_dict()
            }
        
        # Spread distribution
        spread_bins = [0, 2, 5, 10, 20, 50, 100]
        spread_labels = ['0-2%', '2-5%', '5-10%', '10-20%', '20-50%', '50%+']
        
        df['spread_bucket'] = pd.cut(df['spread_pct'], bins=spread_bins, labels=spread_labels, right=False)
        spread_counts = df['spread_bucket'].value_counts()
        analysis['spread_distribution'] = spread_counts.to_dict()
        
        return analysis
    
    def analyze_time_patterns(self) -> Dict[str, Any]:
        """Analyze temporal patterns in arbitrage opportunities"""
        if self.opportunities_df is None or self.opportunities_df.empty:
            return {}
        
        df = self.opportunities_df.copy()
        df['hour'] = df['timestamp'].dt.hour
        df['day_of_week'] = df['timestamp'].dt.dayofweek
        df['date'] = df['timestamp'].dt.date
        
        analysis = {
            'hourly_distribution': df['hour'].value_counts().sort_index().to_dict(),
            'daily_distribution': df['day_of_week'].value_counts().sort_index().to_dict(),
            'opportunities_per_day': df.groupby('date').size().describe().to_dict(),
            'avg_spread_by_hour': df.groupby('hour')['spread_pct'].mean().to_dict(),
            'avg_spread_by_day': df.groupby('day_of_week')['spread_pct'].mean().to_dict()
        }
        
        # Day names for better readability
        day_names = {0: 'Monday', 1: 'Tuesday', 2: 'Wednesday', 3: 'Thursday', 
                    4: 'Friday', 5: 'Saturday', 6: 'Sunday'}
        analysis['daily_distribution_named'] = {
            day_names[k]: v for k, v in analysis['daily_distribution'].items()
        }
        
        return analysis
    
    def analyze_market_conditions(self) -> Dict[str, Any]:
        """Analyze arbitrage opportunities under different market conditions"""
        if self.opportunities_df is None or self.opportunities_df.empty:
            return {}
        
        df = self.opportunities_df.copy()
        analysis = {
            'price_ranges': {},
            'volatility_impact': {},
            'direction_analysis': {}
        }
        
        # Analyze by price ranges for each symbol
        for symbol in df['symbol'].unique():
            symbol_df = df[df['symbol'] == symbol]
            if len(symbol_df) > 0:
                # Create price buckets
                price_col = 'source_price'
                q25, q75 = symbol_df[price_col].quantile([0.25, 0.75])
                
                conditions = [
                    (symbol_df[price_col] <= q25, 'Low Price'),
                    ((symbol_df[price_col] > q25) & (symbol_df[price_col] <= q75), 'Mid Price'),
                    (symbol_df[price_col] > q75, 'High Price')
                ]
                
                price_analysis = {}
                for condition, label in conditions:
                    subset = symbol_df[condition]
                    if len(subset) > 0:
                        price_analysis[label] = {
                            'count': len(subset),
                            'avg_spread': subset['spread_pct'].mean(),
                            'max_spread': subset['spread_pct'].max()
                        }
                
                analysis['price_ranges'][symbol] = price_analysis
        
        # Direction analysis
        direction_stats = df.groupby('direction').agg({
            'spread_pct': ['count', 'mean', 'std', 'max'],
            'symbol': lambda x: x.value_counts().to_dict()
        }).round(2)
        
        analysis['direction_analysis'] = {
            'overvalued_count': len(df[df['direction'] == 'market_overvalued']),
            'undervalued_count': len(df[df['direction'] == 'market_undervalued']),
            'avg_spread_overvalued': df[df['direction'] == 'market_overvalued']['spread_pct'].mean(),
            'avg_spread_undervalued': df[df['direction'] == 'market_undervalued']['spread_pct'].mean()
        }
        
        return analysis
    
    def find_recurring_patterns(self) -> Dict[str, Any]:
        """Find recurring patterns and anomalies"""
        if self.opportunities_df is None or self.opportunities_df.empty:
            return {}
        
        df = self.opportunities_df.copy()
        patterns = {
            'recurring_contracts': {},
            'high_frequency_periods': {},
            'anomalous_spreads': {},
            'symbol_correlations': {}
        }
        
        # Find contracts that appear frequently
        contract_counts = df['contract_id'].value_counts()
        patterns['recurring_contracts'] = contract_counts.head(10).to_dict()
        
        # Find high frequency periods (more than 5 opportunities in an hour)
        df['hour_bucket'] = df['timestamp'].dt.floor('H')
        hourly_counts = df.groupby('hour_bucket').size()
        high_freq = hourly_counts[hourly_counts >= 5]
        patterns['high_frequency_periods'] = {
            str(k): v for k, v in high_freq.to_dict().items()
        }
        
        # Find anomalous spreads (> 95th percentile)
        spread_95th = df['spread_pct'].quantile(0.95)
        anomalous = df[df['spread_pct'] > spread_95th]
        patterns['anomalous_spreads'] = {
            'threshold': spread_95th,
            'count': len(anomalous),
            'examples': anomalous[['timestamp', 'symbol', 'spread_pct', 'question']].head(5).to_dict('records')
        }
        
        # Symbol correlation analysis
        if len(df['symbol'].unique()) > 1:
            symbol_hourly = df.groupby(['hour_bucket', 'symbol']).size().unstack(fill_value=0)
            if len(symbol_hourly.columns) > 1:
                correlation_matrix = symbol_hourly.corr()
                patterns['symbol_correlations'] = correlation_matrix.to_dict()
        
        return patterns
    
    def generate_summary_report(self) -> str:
        """Generate a comprehensive summary report"""
        if self.opportunities_df is None:
            self.load_opportunities()
        
        if self.memory_data is None:
            self.load_memory()
        
        spread_analysis = self.analyze_spread_patterns()
        time_analysis = self.analyze_time_patterns()
        market_analysis = self.analyze_market_conditions()
        pattern_analysis = self.find_recurring_patterns()
        
        report = []
        report.append("=" * 60)
        report.append("ARBITRAGE OPPORTUNITY ANALYSIS REPORT")
        report.append("=" * 60)
        report.append(f"Generated: {datetime.now(timezone.utc).strftime('%Y-%m-%d %H:%M:%S UTC')}")
        report.append("")
        
        # Overall Statistics
        if spread_analysis.get('overall_stats'):
            stats = spread_analysis['overall_stats']
            report.append("OVERALL STATISTICS:")
            report.append(f"  Total Opportunities: {stats['total_opportunities']:,}")
            report.append(f"  Average Spread: {stats['avg_spread']:.2f}%")
            report.append(f"  Median Spread: {stats['median_spread']:.2f}%")
            report.append(f"  Max Spread: {stats['max_spread']:.2f}%")
            report.append(f"  Spread Std Dev: {stats['std_spread']:.2f}%")
            report.append("")
        
        # By Symbol Analysis
        if spread_analysis.get('by_symbol'):
            report.append("BY SYMBOL ANALYSIS:")
            for symbol, data in spread_analysis['by_symbol'].items():
                report.append(f"  {symbol}:")
                report.append(f"    Count: {data['count']:,}")
                report.append(f"    Avg Spread: {data['avg_spread']:.2f}%")
                report.append(f"    Max Spread: {data['max_spread']:.2f}%")
                direction_str = ", ".join([f"{k}: {v}" for k, v in data['direction_counts'].items()])
                report.append(f"    Directions: {direction_str}")
            report.append("")
        
        # Time Patterns
        if time_analysis.get('hourly_distribution'):
            report.append("TIME PATTERNS:")
            
            # Find peak hours
            hourly = time_analysis['hourly_distribution']
            peak_hour = max(hourly, key=hourly.get)
            report.append(f"  Peak Hour: {peak_hour}:00 UTC ({hourly[peak_hour]} opportunities)")
            
            # Find peak day
            daily = time_analysis.get('daily_distribution_named', {})
            if daily:
                peak_day = max(daily, key=daily.get)
                report.append(f"  Peak Day: {peak_day} ({daily[peak_day]} opportunities)")
            
            # Daily average
            daily_stats = time_analysis.get('opportunities_per_day', {})
            if 'mean' in daily_stats:
                report.append(f"  Avg Opportunities/Day: {daily_stats['mean']:.1f}")
            report.append("")
        
        # Market Conditions
        if market_analysis.get('direction_analysis'):
            direction = market_analysis['direction_analysis']
            report.append("MARKET DIRECTION ANALYSIS:")
            report.append(f"  Market Overvalued: {direction['overvalued_count']:,} opportunities")
            report.append(f"  Market Undervalued: {direction['undervalued_count']:,} opportunities")
            if 'avg_spread_overvalued' in direction:
                report.append(f"  Avg Spread (Overvalued): {direction['avg_spread_overvalued']:.2f}%")
            if 'avg_spread_undervalued' in direction:
                report.append(f"  Avg Spread (Undervalued): {direction['avg_spread_undervalued']:.2f}%")
            report.append("")
        
        # Patterns
        if pattern_analysis.get('recurring_contracts'):
            report.append("RECURRING PATTERNS:")
            recurring = pattern_analysis['recurring_contracts']
            report.append(f"  Most Active Contract: {list(recurring.keys())[0]} ({list(recurring.values())[0]} times)")
            
            if pattern_analysis.get('high_frequency_periods'):
                high_freq = pattern_analysis['high_frequency_periods']
                report.append(f"  High Frequency Periods: {len(high_freq)}")
            
            if pattern_analysis.get('anomalous_spreads'):
                anomalous = pattern_analysis['anomalous_spreads']
                report.append(f"  Anomalous Spreads (>{anomalous['threshold']:.1f}%): {anomalous['count']}")
            report.append("")
        
        # Memory Statistics
        if self.memory_data:
            report.append("HISTORICAL MEMORY:")
            report.append(f"  Total Historical Opportunities: {self.memory_data.get('total_opportunities', 0):,}")
            
            if 'largest_spreads' in self.memory_data:
                report.append("  Largest Historical Spreads:")
                for symbol, spread in self.memory_data['largest_spreads'].items():
                    report.append(f"    {symbol}: {spread:.2f}%")
            report.append("")
        
        report.append("=" * 60)
        
        return "\n".join(report)
    
    def export_to_csv(self, filename: Optional[str] = None) -> str:
        """Export opportunities to CSV file"""
        if self.opportunities_df is None or self.opportunities_df.empty:
            return "No data to export"
        
        if filename is None:
            timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
            filename = f"arbitrage_opportunities_{timestamp}.csv"
        
        filepath = self.log_dir / filename
        self.opportunities_df.to_csv(filepath, index=False)
        return f"Exported {len(self.opportunities_df)} opportunities to {filepath}"


def main():
    """Main entry point for the analyzer"""
    analyzer = ArbitrageAnalyzer()
    
    print("Loading arbitrage opportunities...")
    df = analyzer.load_opportunities(days_back=30)
    
    if df.empty:
        print("No opportunities found in the last 30 days.")
        return
    
    print(f"Loaded {len(df)} opportunities")
    print("\nGenerating analysis report...")
    
    # Generate and print report
    report = analyzer.generate_summary_report()
    print(report)
    
    # Export to CSV
    export_msg = analyzer.export_to_csv()
    print(f"\n{export_msg}")
    
    # Save report to file
    report_file = analyzer.log_dir / f"analysis_report_{datetime.now().strftime('%Y%m%d_%H%M%S')}.txt"
    with open(report_file, 'w') as f:
        f.write(report)
    print(f"Report saved to {report_file}")


if __name__ == "__main__":
    main()