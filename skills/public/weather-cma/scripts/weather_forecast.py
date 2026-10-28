#!/usr/bin/env python3
"""Read dated Chinese forecasts from complete official pages, without API keys."""

import argparse
from datetime import date, datetime, timedelta, timezone
from html.parser import HTMLParser
import json
import re
import urllib.error
import urllib.parse
import urllib.request

SHANGHAI = timezone(timedelta(hours=8), 'Asia/Shanghai')
MAX_BODY = 2 * 1024 * 1024
CITIES = {
    '北京': ('101010100', 'ABJ/beijing'),
    '上海': ('101020100', 'ASH/xujiahui'),
    '广州': ('101280101', 'AGD/guangzhou'),
    '深圳': ('101280601', 'AGD/shenzuo'),
    '成都': ('101270101', 'ASC/chengdu'),
    '杭州': ('101210101', 'AZJ/hangzhou'),
    '武汉': ('101200101', 'AHB/wuhan'),
    '南京': ('101190101', 'AJS/nanjing'),
    '天津': ('101030100', 'ATJ/heiniucheng'),
    '重庆': ('101040100', 'ACQ/shapingba'),
    '西安': ('101110101', 'ASN/xian'),
}
VOID = {'area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input',
        'link', 'meta', 'param', 'source', 'track', 'wbr'}


class ForecastError(Exception):
    pass


class Node:
    def __init__(self, tag='', attrs=()):
        self.tag, self.attrs, self.children = tag, dict(attrs), []

    def text(self):
        return ' '.join(' '.join(c.text() if isinstance(c, Node) else c
                               for c in self.children).split())

    def find(self, tag=None, cls=None, id=None):
        for child in self.children:
            if not isinstance(child, Node):
                continue
            if ((tag is None or child.tag == tag)
                    and (cls is None or cls in child.attrs.get('class', '').split())
                    and (id is None or child.attrs.get('id') == id)):
                yield child
            yield from child.find(tag, cls, id)


class Document(HTMLParser):
    def __init__(self, text):
        super().__init__(convert_charrefs=True)
        self.root = Node()
        self.stack = [self.root]
        self.feed(text)

    def handle_starttag(self, tag, attrs):
        node = Node(tag, attrs)
        self.stack[-1].children.append(node)
        if tag not in VOID:
            self.stack.append(node)

    def handle_startendtag(self, tag, attrs):
        self.stack[-1].children.append(Node(tag, attrs))

    def handle_endtag(self, tag):
        for index in range(len(self.stack) - 1, 0, -1):
            if self.stack[index].tag == tag:
                del self.stack[index:]
                break

    def handle_data(self, text):
        self.stack[-1].children.append(text)


def first(items, error):
    value = next(iter(items), None)
    if value is None:
        raise ForecastError(error)
    return value


def verify_page(text, city):
    if any(marker in text for marker in (
            'WEB 应用防火墙', '人机识别检测', '向右滑动填充拼图', '请完成安全验证')):
        raise ForecastError('human_verification_required')
    document = Document(text).root
    title = first(document.find('title'), 'missing_page_title').text()
    if city not in title:
        raise ForecastError('city_does_not_match_page')
    return document


def validate_publication(published, now):
    # Evening forecast pages may be labelled with their coming issue hour.
    age = now - published
    if age > timedelta(hours=36) or age < -timedelta(hours=6):
        raise ForecastError('forecast_publication_is_not_current')


def temperature(node):
    if node is None or not node.text().strip():
        return None
    match = re.fullmatch(r'(-?\d+(?:\.\d+)?)\s*(?:℃|°C)?', node.text().strip())
    if not match:
        raise ForecastError('invalid_temperature')
    value = float(match[1])
    if not -90 <= value <= 60:
        raise ForecastError('temperature_out_of_range')
    return int(value) if value.is_integer() else value


def forecast_entry(day, condition, high, low, wind, daytime=None, nighttime=None):
    if not condition or len(condition) > 60 or low is None:
        raise ForecastError('incomplete_forecast')
    if high is not None and high < low:
        raise ForecastError('invalid_temperature_range')
    return {'date': day.isoformat(), 'condition': condition,
            'high_c': high, 'low_c': low, 'wind': wind,
            'daytime': daytime, 'nighttime': nighttime,
            'rain_expected': bool(re.search('雨|冰雹', condition)),
            'snow_expected': bool(re.search('雪|霰', condition))}


def parse_weathercom(text, city, now):
    document = verify_page(text, city)
    section = first(document.find(id='7d'), 'missing_seven_day_forecast')
    stamp = first(section.find('input', id='fc_24h_internal_update_time'),
                  'missing_forecast_publication').attrs.get('value', '')
    if not re.fullmatch(r'\d{10}', stamp):
        raise ForecastError('invalid_forecast_publication')
    anchor = datetime.strptime(stamp, '%Y%m%d%H').replace(tzinfo=SHANGHAI)
    update = first(section.find('input', id='update_time'),
                   'missing_forecast_publication').attrs.get('value', '')
    if not re.fullmatch(r'\d{2}:\d{2}', update):
        raise ForecastError('invalid_forecast_publication')
    hour, minute = map(int, update.split(':'))
    published = anchor.replace(hour=hour, minute=minute)
    validate_publication(published, now)
    rows = list(section.find('li', cls='sky'))
    if not 2 <= len(rows) <= 7:
        raise ForecastError('invalid_forecast_row_count')
    forecasts = []
    for index, row in enumerate(rows):
        day = anchor.date() + timedelta(days=index)
        heading = first(row.find('h1'), 'missing_forecast_date').text()
        match = re.match(r'(\d{1,2})日', heading)
        if not match or int(match[1]) != day.day:
            raise ForecastError('forecast_date_sequence_mismatch')
        condition = first(row.find('p', cls='wea'), 'missing_condition').text()
        temp = first(row.find('p', cls='tem'), 'missing_temperature')
        high = temperature(next(temp.find('span'), None))
        low = temperature(next(temp.find('i'), None))
        win = first(row.find('p', cls='win'), 'missing_wind')
        directions = list(dict.fromkeys(n.attrs.get('title', '') for n in win.find('span')))
        wind = '转'.join(x for x in directions if x) + ' ' + win.text()
        parts = condition.split('转')
        daytime, nighttime = (parts if len(parts) == 2 else (condition, condition))
        forecasts.append(forecast_entry(day, condition, high, low, wind.strip(),
                                        daytime, nighttime))
    return published, forecasts


def parse_nmc(text, city, now):
    document = verify_page(text, city)
    # Use the daily forecast timestamp rather than hourly/news/article dates.
    match = re.search(r'发布时间[：:]\s*(\d{2}-\d{2})\s+(\d{2}:\d{2})', text)
    if not match:
        raise ForecastError('missing_forecast_publication')
    candidates = []
    for year in (now.year - 1, now.year, now.year + 1):
        try:
            candidates.append(datetime.strptime(f'{year}-{match[1]} {match[2]}',
                                                 '%Y-%m-%d %H:%M').replace(tzinfo=SHANGHAI))
        except ValueError:
            pass
    if not candidates:
        raise ForecastError('invalid_forecast_publication')
    published = min(candidates, key=lambda value: abs((value - now).total_seconds()))
    validate_publication(published, now)
    section = first(document.find(id='day7'), 'missing_seven_day_forecast')
    rows = list(section.find('div', cls='weather'))
    if not 2 <= len(rows) <= 7:
        raise ForecastError('invalid_forecast_row_count')
    forecasts = []
    for index, row in enumerate(rows):
        day = published.date() + timedelta(days=index)
        heading = first(row.find('div', cls='date'), 'missing_forecast_date').text()
        match = re.match(r'(\d{1,2})/(\d{1,2})', heading)
        if not match or (int(match[1]), int(match[2])) != (day.month, day.day):
            raise ForecastError('forecast_date_sequence_mismatch')
        descriptions = [n.text() for n in row.find('div', cls='desc')]
        temps = [temperature(n) for n in row.find('div', cls='tmp')]
        if len(descriptions) != 2 or len(temps) != 2:
            raise ForecastError('incomplete_forecast')
        daytime, nighttime = descriptions
        condition = daytime if daytime == nighttime else '转'.join(x for x in descriptions if x)
        directions = [n.text() for n in row.find('div', cls='windd')]
        speeds = [n.text() for n in row.find('div', cls='winds')]
        wind = '；'.join(dict.fromkeys(' '.join(x).strip() for x in zip(directions, speeds)))
        forecasts.append(forecast_entry(day, condition, temps[0], temps[1], wind,
                                        daytime or None, nighttime or None))
    return published, forecasts


class OfficialRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        parsed = urllib.parse.urlsplit(newurl)
        if parsed.scheme != 'https' or parsed.hostname not in ('www.weather.com.cn', 'www.nmc.cn'):
            raise ForecastError('unexpected_source_redirect')
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def fetch_page(url, timeout):
    request = urllib.request.Request(url, headers={
        'User-Agent': 'DeerFlow-Weather/1.0', 'Accept': 'text/html', 'Cache-Control': 'no-cache'})
    with urllib.request.build_opener(OfficialRedirect()).open(request, timeout=timeout) as response:
        raw = response.read(MAX_BODY + 1)
        if len(raw) > MAX_BODY:
            raise ForecastError('source_page_too_large')
        charset = response.headers.get_content_charset() or 'utf-8'
        return raw.decode(charset, errors='strict')


def target_date(value, today):
    relative = {'today': 0, '今天': 0, 'tomorrow': 1, '明天': 1,
                'after-tomorrow': 2, '后天': 2}
    if value in relative:
        return today + timedelta(days=relative[value])
    try:
        result = date.fromisoformat(value)
    except ValueError as error:
        raise ForecastError('date_must_be_today_tomorrow_or_YYYY-MM-DD') from error
    if not today <= result <= today + timedelta(days=6):
        raise ForecastError('date_outside_current_seven_day_forecast')
    return result


def query(city='北京', requested='tomorrow', days=1, code=None, timeout=8, *, now=None, fetch=fetch_page):
    now = (now or datetime.now(SHANGHAI)).astimezone(SHANGHAI)
    city = city.removesuffix('市')
    target = target_date(requested, now.date())
    end = target + timedelta(days=days - 1)
    if end > now.date() + timedelta(days=6):
        raise ForecastError('date_outside_current_seven_day_forecast')
    known = CITIES.get(city)
    if code and not re.fullmatch(r'\d{9}', code):
        raise ForecastError('weather_code_must_be_nine_digits')
    if not known and not code:
        raise ForecastError('city_requires_verified_weather_code')
    if known and code and known[0] != code:
        raise ForecastError('weather_code_does_not_match_city')
    code = code or known[0]
    sources = [('中国天气网', f'https://www.weather.com.cn/weather/{code}.shtml', parse_weathercom)]
    if known:
        sources.append(('国家气象中心', f'https://www.nmc.cn/publish/forecast/{known[1]}.html', parse_nmc))
    attempts = []
    for name, url, parser in sources:
        try:
            published, forecasts = parser(fetch(url, timeout), city, now)
            selected = [item for item in forecasts if target.isoformat() <= item['date'] <= end.isoformat()]
            if len(selected) != days or any(item['high_c'] is None and item['date'] > now.date().isoformat()
                                            for item in selected):
                raise ForecastError('requested_dates_missing_from_forecast')
            return {'ok': True, 'city': city, 'timezone': 'Asia/Shanghai',
                    'checked_at': now.isoformat(timespec='seconds'), 'today': now.date().isoformat(),
                    'requested_date': target.isoformat(), 'source': name, 'source_url': url,
                    'source_published_at': published.isoformat(timespec='minutes'),
                    'forecast': selected, 'fallback_attempts': attempts}
        except (ForecastError, urllib.error.URLError, TimeoutError, UnicodeError, ValueError) as error:
            reason = str(error) if isinstance(error, ForecastError) else type(error).__name__
            attempts.append({'source': name, 'reason': reason})
    return {'ok': False, 'city': city, 'timezone': 'Asia/Shanghai',
            'today': now.date().isoformat(), 'requested_date': target.isoformat(),
            'error': 'no_current_verified_forecast', 'attempts': attempts}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--city', default='北京')
    parser.add_argument('--date', default='tomorrow', help='today, tomorrow, after-tomorrow, or YYYY-MM-DD')
    parser.add_argument('--days', type=int, choices=range(1, 8), default=1)
    parser.add_argument('--weather-code', help='Verified nine-digit weather.com.cn city code for an unlisted city')
    parser.add_argument('--timeout', type=int, choices=range(3, 13), default=8)
    args = parser.parse_args(argv)
    try:
        result = query(args.city, args.date, args.days, args.weather_code, args.timeout)
    except ForecastError as error:
        result = {'ok': False, 'error': str(error)}
    print(json.dumps(result, ensure_ascii=False))
    return 0 if result['ok'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
