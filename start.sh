#!/bin/bash
set -e

echo "=================================================="
echo "🚀 Starting High-Performance Go Worker (2GB Engine)"
echo "FFmpeg Version: $(ffmpeg -version | head -n 1 2>/dev/null || echo 'OK')"
echo "=================================================="

# مشخصات تلگرام API برای سرور محلی (پیش‌فرض رسمی)
TG_API_ID="${TELEGRAM_API_ID:-2040}"
TG_API_HASH="${TELEGRAM_API_HASH:-b18441a1ff607e10a989891a5462e627}"

# ساخت دایرکتوری‌های داده تلگرام و دانلودها
mkdir -p /app/storage/logs /app/storage/tgdata /app/storage/tgdata/temp /app/storage/downloads
chmod -R 777 /app/storage

# ۱. اجرای Local Telegram Bot API Server در پس‌زمینه (پورت 8081 برای آپلود تا ۲ گیگابایت)
echo "⚡ Starting Local Telegram Bot API Server (2000MB limit enabled)..."
telegram-bot-api \
    --api-id="${TG_API_ID}" \
    --api-hash="${TG_API_HASH}" \
    --local \
    --http-port=8081 \
    --max-connections=1000 \
    --dir=/app/storage/tgdata \
    --temp-dir=/app/storage/tgdata/temp &
sleep 2

# ۲. پروسه پاکسازی فایل‌های موقت در پس‌زمینه
(
    while true; do
        sleep 300
        find /app/storage/downloads -type f -mmin +15 -delete 2>/dev/null || true
        find /app/storage/tgdata/temp -type f -mmin +15 -delete 2>/dev/null || true
    done
) &

# ۳. اجرای باینری نیتیو ورکر Go
LISTEN_PORT="${PORT:-8080}"
export GOMEMLIMIT="${GOMEMLIMIT:-380MiB}"
export GOGC="${GOGC:-40}"

echo "🌐 Starting Ultra-Fast Go Worker on port ${LISTEN_PORT} (GOMEMLIMIT=${GOMEMLIMIT})..."
exec /app/worker

