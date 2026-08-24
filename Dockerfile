# ==============================================================================
# Ultra-Lightweight Multi-Stage Dockerfile for High-Speed Go Worker (2GB Upload)
# ==============================================================================

# مرحله اول: کامپایل باینری Go
FROM golang:1.22-alpine AS builder
WORKDIR /build
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o worker .

# مرحله دوم: باینری سرور تلگرام برای آپلود تا ۲ گیگابایت
FROM aiogram/telegram-bot-api:latest AS bot_api_binary

# مرحله سوم: محیط اجرایی مینیمال Alpine
FROM alpine:3.19

# کپی باینری سرور تلگرام
COPY --from=bot_api_binary /usr/local/bin/telegram-bot-api /usr/local/bin/telegram-bot-api

# کپی باینری کامپایل‌شده ورکر
COPY --from=builder /build/worker /app/worker

# نصب FFmpeg، Python3، yt-dlp و بسته‌های سیستمی ضروری
RUN apk add --no-cache \
    curl \
    ca-certificates \
    ffmpeg \
    python3 \
    bash \
    dos2unix \
    tzdata \
    libstdc++ \
    libssl3 \
    libcrypto3 \
    zlib \
    && curl -L https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp \
    && chmod a+rx /usr/local/bin/yt-dlp \
    && rm -rf /var/cache/apk/*

WORKDIR /app

# کپی اسکریپت راه‌اندازی
COPY start.sh /app/start.sh

# ساخت دایرکتوری‌های کش و ذخیره‌سازی داده
RUN mkdir -p /app/storage/downloads /app/storage/tgdata /app/storage/tgdata/temp \
    && chmod -R 777 /app/storage \
    && dos2unix /app/start.sh \
    && chmod +x /app/start.sh /app/worker

ENV PORT=8080
ENV GOMEMLIMIT=380MiB
ENV GOGC=40

EXPOSE 8080 8081

ENTRYPOINT ["/app/start.sh"]

