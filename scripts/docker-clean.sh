#!/bin/bash

echo "Stopping all containers..."
docker stop $(docker ps -aq) 2>/dev/null || true

echo "Removing all containers..."
docker rm -f $(docker ps -aq) 2>/dev/null || true

echo "Removing all images..."
docker rmi -f $(docker images -aq) 2>/dev/null || true

echo "Removing all volumes..."
docker volume rm $(docker volume ls -q) 2>/dev/null || true

echo "Removing all custom networks..."
docker network rm $(docker network ls -q --filter type=custom) 2>/dev/null || true

echo "Cleaning builder cache..."
docker builder prune -af

echo "Performing system prune..."
docker system prune -af --volumes

echo "Docker cleanup complete."