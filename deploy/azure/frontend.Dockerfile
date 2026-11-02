# Build with --build-context frontend=./frontend; root .dockerignore excludes it.
FROM node:22.21.1-bookworm-slim AS build
WORKDIR /app
COPY --from=frontend package.json pnpm-lock.yaml ./
RUN npm install --global pnpm@10.33.3 && pnpm install --frozen-lockfile
COPY --from=frontend . .
RUN pnpm build
FROM nginxinc/nginx-unprivileged:1.28.0-alpine
COPY deploy/azure/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
