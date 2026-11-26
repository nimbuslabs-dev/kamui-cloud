FROM nginx:stable-alpine
RUN apk add --no-cache openssl
COPY workspace-proxy.conf /etc/nginx/conf.d/default.conf
COPY generate-cert.sh /usr/local/share/kamui/generate-cert.sh
