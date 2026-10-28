"""Date anchoring and fallback tests for the portable weather skill helper."""

from datetime import date, datetime, timedelta, timezone
import importlib.util
from pathlib import Path
import unittest

SOURCE = Path(__file__).resolve().parents[1] / 'skills/public/weather-cma/scripts/weather_forecast.py'
spec = importlib.util.spec_from_file_location('weather_forecast', SOURCE)
weather = importlib.util.module_from_spec(spec)
spec.loader.exec_module(weather)


def weathercom_page(start, *, city='北京', bad_index=None, missing_high=False):
    rows = []
    for offset in range(3):
        day = start.date() + timedelta(days=offset)
        label = day.day if offset != bad_index else day.day % 28 + 1
        high = '' if missing_high else '<span>12℃</span>/'
        rows.append(f'<li class="sky"><h1>{label}日</h1>'
                    f'<p class="wea">小雨</p><p class="tem">{high}<i>9℃</i></p>'
                    '<p class="win"><span title="北风"></span><i>&lt;3级</i></p></li>')
    return (f'<title>{city}天气预报</title><nav>{"导航" * 3000}</nav><div id="7d">'
            f'<input id="fc_24h_internal_update_time" value="{start:%Y%m%d%H}">'
            '<input id="update_time" value="18:00">'
            f'<ul>{"".join(rows)}</ul></div>')


def nmc_page(start, *, bad_index=None):
    rows = []
    for offset in range(3):
        day = start.date() + timedelta(days=offset)
        label = f'{day:%m/%d}' if offset != bad_index else '04/16'
        rows.append(f'<div class="weather"><div class="date">{label}</div>'
                    '<div class="desc">小雨</div><div class="desc">阴</div>'
                    '<div class="tmp">12℃</div><div class="tmp">9℃</div>'
                    '<div class="windd">北风</div><div class="winds">微风</div>'
                    '<div class="windd">北风</div><div class="winds">微风</div></div>')
    return (f'<title>北京-天气预报</title><div>发布时间：{start:%m-%d %H:%M}</div>'
            f'<div id="day7">{"".join(rows)}</div>')


class WeatherForecastTests(unittest.TestCase):
    def setUp(self):
        self.now = datetime(2026, 10, 24, 17, 30, tzinfo=weather.SHANGHAI)
        self.issue = self.now.replace(hour=20, minute=0)

    def test_full_html_finds_forecast_after_large_navigation(self):
        result = weather.query(now=self.now, fetch=lambda url, timeout: weathercom_page(self.issue))
        self.assertTrue(result['ok'])
        self.assertEqual(result['requested_date'], '2026-10-25')
        self.assertEqual(result['forecast'][0]['high_c'], 12)
        self.assertTrue(result['forecast'][0]['rain_expected'])
        self.assertEqual(len(result['forecast']), 1)

    def test_midnight_uses_shanghai_clock_and_old_issue_date_not_row_label(self):
        utc_now = datetime(2026, 10, 24, 16, 1, tzinfo=timezone.utc)
        result = weather.query(now=utc_now, fetch=lambda url, timeout: weathercom_page(self.issue))
        self.assertEqual(result['today'], '2026-10-25')
        self.assertEqual(result['forecast'][0]['date'], '2026-10-26')

    def test_month_and_year_rollover_use_publication_anchor(self):
        now = datetime(2026, 12, 31, 21, tzinfo=weather.SHANGHAI)
        result = weather.query(now=now, fetch=lambda url, timeout: weathercom_page(now))
        self.assertEqual(result['forecast'][0]['date'], '2027-01-01')

    def test_bad_date_sequence_does_not_become_current_forecast(self):
        with self.assertRaisesRegex(weather.ForecastError, 'sequence_mismatch'):
            weather.parse_weathercom(weathercom_page(self.issue, bad_index=1), '北京', self.now)

    def test_stale_april_page_is_rejected_in_october(self):
        april = self.issue.replace(month=4, day=15)
        result = weather.query(now=self.now, fetch=lambda url, timeout: weathercom_page(april))
        self.assertFalse(result['ok'])
        self.assertNotIn('forecast', result)
        self.assertEqual(result['attempts'][0]['reason'], 'forecast_publication_is_not_current')

    def test_waf_falls_back_to_dated_nmc_data(self):
        def fetch(url, timeout):
            if 'weather.com.cn' in url:
                return '<title>WEB 应用防火墙</title>向右滑动填充拼图'
            return nmc_page(self.issue)
        result = weather.query(now=self.now, fetch=fetch)
        self.assertTrue(result['ok'])
        self.assertEqual(result['source'], '国家气象中心')
        self.assertEqual(result['fallback_attempts'][0]['reason'], 'human_verification_required')
        self.assertEqual(result['forecast'][0]['condition'], '小雨转阴')

    def test_nmc_year_rollover_uses_nearest_issue_year(self):
        now = datetime(2027, 1, 1, 1, tzinfo=weather.SHANGHAI)
        issue = datetime(2026, 12, 31, 20, tzinfo=weather.SHANGHAI)
        published, rows = weather.parse_nmc(nmc_page(issue), '北京', now)
        self.assertEqual(published.year, 2026)
        self.assertEqual(rows[1]['date'], '2027-01-01')

    def test_nmc_date_mismatch_is_rejected(self):
        with self.assertRaisesRegex(weather.ForecastError, 'sequence_mismatch'):
            weather.parse_nmc(nmc_page(self.issue, bad_index=1), '北京', self.now)

    def test_all_sources_blocked_has_no_invented_forecast(self):
        result = weather.query(now=self.now, fetch=lambda url, timeout: '人机识别检测')
        self.assertFalse(result['ok'])
        self.assertEqual(len(result['attempts']), 2)
        self.assertNotIn('forecast', result)

    def test_missing_future_high_temperature_is_not_complete(self):
        result = weather.query(now=self.now,
                               fetch=lambda url, timeout: weathercom_page(self.issue, missing_high=True))
        self.assertFalse(result['ok'])

    def test_explicit_old_date_and_wrong_city_fail_before_claiming_weather(self):
        with self.assertRaisesRegex(weather.ForecastError, 'outside_current'):
            weather.query(requested='2026-04-16', now=self.now)
        with self.assertRaisesRegex(weather.ForecastError, 'city_does_not_match'):
            weather.parse_weathercom(weathercom_page(self.issue, city='上海'), '北京', self.now)

    def test_verified_city_code_cannot_rebind_known_city(self):
        with self.assertRaisesRegex(weather.ForecastError, 'does_not_match_city'):
            weather.query(code='101020100', now=self.now)


if __name__ == '__main__':
    unittest.main()
