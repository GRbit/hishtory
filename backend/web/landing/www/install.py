"""
A small install script to download the correct hishtory binary for the current OS/architecture.
The hishtory binary is in charge of installing itself, this just downloads the correct binary and
executes it.
"""

import json
import urllib.request
import platform
import sys
import os

with urllib.request.urlopen('https://api.hishtory.dev/api/v1/download') as response:
    resp_body = response.read()
download_options = json.loads(resp_body)

if platform.system() == 'Linux':
    download_url = download_options['linux_amd_64_url']
elif platform.system() == 'Darwin' and platform.machine() == 'arm64':
    download_url = download_options['darwin_arm_64_url']
elif platform.system() == 'Darwin' and platform.machine() == 'x86_64':
    download_url = download_options['darwin_amd_64_url']
else:
    print(f"No hishtory binary for system={platform.system()}, machine={platform.machine()}!\nIf you believe this is a mistake, please open an issue here: https://github.com/ddworken/hishtory/issues")
    sys.exit(1)

with urllib.request.urlopen(download_url) as response:
    hishtory_binary = response.read()
with open('/tmp/hishtory-client', 'wb') as f:
    f.write(hishtory_binary)
os.system('chmod +x /tmp/hishtory-client')
os.system('/tmp/hishtory-client install')
# TODO: Detect if ^ failed
print('Succesfully installed hishtory! Open a new terminal, try running a command, and then running `hishtory query`.')
