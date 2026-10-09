# The Jokku website: built with Node, then served as static files by Caddy.
# On a Jokku server, deploy it from the repo root with build-dir set to
# website (see README.md).

FROM node:24-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM caddy:2-alpine
COPY Caddyfile /etc/caddy/Caddyfile
COPY --from=build /app/dist/public /srv
# The newest EXPOSE is the port Jokku routes to; the Caddyfile listens on
# $PORT, which Jokku sets to it.
EXPOSE 8080
