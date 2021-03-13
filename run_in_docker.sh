#!/bin/bash

# This looks like it runs the build twice, but the second invocation is cached.
# (The point of the first invocation is to show build progress on first run.)
docker build . &&
docker run -v $HOME/.watchurl:/mnt/data -it --rm $(docker build -q .) "$@"
