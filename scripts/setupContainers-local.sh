#!/bin/bash

set -e

if [ -z $SWARM_NET ]; then
  echo "Pls specify env var SWARM_NET"
  exit
fi

if [ -z $DOCKER_IMAGE ]; then
  echo "Pls specify env var DOCKER_IMAGE"
  exit
fi

if [ -z $IPS_FILE ]; then
  echo "Pls specify env var IPS_FILE"
  exit
fi

if [ -z $LATENCY_MAP ]; then
  echo "Pls specify env var LATENCY_MAP"
  exit
fi



echo "SWARM_NET: $SWARM_NET"
echo "DOCKER_IMAGE: $DOCKER_IMAGE"
echo "IPS_FILE: $IPS_FILE"

echo "Building images..."

currdir=$(pwd)

docker ps -a | awk '{ print $1,$2 }' | grep $DOCKER_IMAGE | awk '{print $1 }' | xargs -I {} docker rm -f {}
sleep 2s
docker network create -d overlay --attachable --subnet $SWARM_SUBNET $SWARM_NET || true

echo "Deploying with config file:"
nContainers=$(wc -l $IPS_FILE)
echo "Lauching containers..."
i=0

while read -r ip name
do
  echo "Starting container with ip $ip and name: $name"
  docker run --net $SWARM_NET -v $SWARM_VOL_DIR:/tmp/logs -d -t --name "node$i" --ip $ip $DOCKER_IMAGE /go/bin/hyparview --bootstraps=10.10.255.254:1200 -listenIP=$ip > output.txt
  i=$((i+1))
done < "$IPS_FILE"