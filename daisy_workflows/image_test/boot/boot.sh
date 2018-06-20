#!/bin/sh
# Copyright 2018 Google Inc. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

if ! ls /reboot.txt; then
  logger -p daemon.info "BOOTED"
  echo "REBOOT" > /reboot.txt
  # Wait a bit for logger
  sleep 10
  shutdown -r now
else
  cat /reboot.txt | logger -p daemon.info
fi
