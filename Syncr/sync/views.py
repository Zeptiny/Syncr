import random
from django.http import HttpResponse
from time import sleep
from django.shortcuts import render
from django.views import View
import json

import subprocess
import threading
import requests

from . import utils
from . import models

class IndexView(View):
    def get(self, request):
        # Listing files in the remote
        # listing = subprocess.run(["rclone", "rc",
        #                          "operations/list",
        #                          "fs=:s3,access_key_id=383f6df5fba416fd4e81c5bd4b6dcc5d,secret_access_key=f642f1f0b7b30b7e6da6aa0a767d69e689296ac3bcdff06c528d201d8914df5d,region=auto,endpoint='https://5bd9c44a6593fe4c511716bc21c06441.r2.cloudflarestorage.com':",
        #                          "remote=rclone-django-testing",
        #                          "--rc-addr=127.0.0.1:5572"])
        # print(listing)
        
        # Copying a directory
        # Local to remote (Only testing)
        # copy = subprocess.run(["rclone", "rc",
        #                          "sync/copy",
        #                          "srcFs=:local:/home/nyuu/test2-2",
        #                          "dstFs=:s3,access_key_id=383f6df5fba416fd4e81c5bd4b6dcc5d,secret_access_key=f642f1f0b7b30b7e6da6aa0a767d69e689296ac3bcdff06c528d201d8914df5d,region=auto,endpoint='https://5bd9c44a6593fe4c511716bc21c06441.r2.cloudflarestorage.com':rclone-django-testing/local",
        #                          "--rc-addr=127.0.0.1:5572",
        #                          "_async=true"],
        #                         check=True,
        #                         stdout=subprocess.PIPE,
        #                         stderr=subprocess.PIPE,)
        
        # jsonTestRemote = {
        #     "type": "s3",
        #     "parameters": {
        #         "access_key_id": "383f6df5fba416fd4e81c5bd4b6dcc5d",
        #         "secret_access_key": "f642f1f0b7b30b7e6da6aa0a767d69e689296ac3bcdff06c528d201d8914df5d",
        #         "region": "auto",
        #         "endpoint": "https://5bd9c44a6593fe4c511716bc21c06441.r2.cloudflarestorage.com"
        #     },
        #     # Formatter specific options
        #     "bucket": "rclone-django-testing/local" # It's not an actual option
        # }
        
        # Test converter
        jsonTestRemoteFormatted = utils.createOnTheFlyRemote(models.Remote.objects.get(pk=1).config)

        print(jsonTestRemoteFormatted)
        copy = requests.post("http://127.0.0.1:5572/sync/copy", json={
            "srcFs": ":local:/home/nyuu/test2-2",
            "dstFs": jsonTestRemoteFormatted,
            #"dstFs": ":s3,access_key_id=383f6df5fba416fd4e81c5bd4b6dcc5d,secret_access_key=f642f1f0b7b30b7e6da6aa0a767d69e689296ac3bcdff06c528d201d8914df5d,region=auto,endpoint='https://5bd9c44a6593fe4c511716bc21c06441.r2.cloudflarestorage.com':rclone-django-testing/local",
            "_async": "true"
        })
        
        # Check for errors
        copy.raise_for_status()
        
        # Remote to remote (Testing server-side transfers)
        # Edit: Cloudflare R2 does not allow server-side transfers
        # copy = subprocess.run(["rclone", "rc",
        #                          "sync/copy",
        #                          "srcFs=:s3,access_key_id=383f6df5fba416fd4e81c5bd4b6dcc5d,secret_access_key=f642f1f0b7b30b7e6da6aa0a767d69e689296ac3bcdff06c528d201d8914df5d,region=auto,endpoint='https://5bd9c44a6593fe4c511716bc21c06441.r2.cloudflarestorage.com':rclone-django-testing",
        #                          "dstFs=:s3,access_key_id=d02c6df77e4bc660dbb43675bf7fcdcb,secret_access_key=3b6399f889a4133bfc767cbbccba3a5f5b527e4eb5b2db6312df0738534d70fa,region=auto,endpoint='https://5bd9c44a6593fe4c511716bc21c06441.r2.cloudflarestorage.com':rclone-django-testing-2",
        #                          "--rc-addr=127.0.0.1:5572",
        #                          "_async=true"],
        #                         check=True,
        #                         stdout=subprocess.PIPE,
        #                         stderr=subprocess.PIPE,)
        # print(copy)
        # With _async=true it return a jobId that can be checked the status with "rclone rc core/stats group=job/JOB_ID --rc-addr=127.0.0.1:5572"
        sleep(10)
        jobId = copy.json().get("jobid")
        print(f"Job ID: {jobId}")
        
        jobObject = utils.createJobObject(jobId, request)
        threading.Thread(target=utils.autoQueryRunningJob, args=(jobObject,)).start()
        
        # status = subprocess.run(["rclone", "rc",
        #                          "core/stats",
        #                          "group=job/" + str(jobId),
        #                          "--rc-addr=127.0.0.1:5572"],
        #                         check=True,
        #                         stdout=subprocess.PIPE,
        #                         stderr=subprocess.PIPE,)
        status = requests.post("http://127.0.0.1:5572/core/stats", json={
            "group": "job/" + str(jobId)
        })
        status.raise_for_status()
        print(status.json())
        
        return render(request, 'sync/index.html')
    
class jobStatus(View):
    def get(self, request, jobId):
        # status = subprocess.run(["rclone", "rc",
        #                          "job/status",
        #                          "jobid=" + str(jobId),
        #                          "--rc-addr=127.0.0.1:5572"],
        #                         check=True,
        #                         stdout=subprocess.PIPE,
        #                         stderr=subprocess.PIPE,)
        status = requests.post("http://127.0.0.1:5572/job/status", json={
            "jobid": jobId
        })
        status.raise_for_status()
        
class jobStats(View):
    def get(self, request, jobId):
        # stats = subprocess.run(["rclone", "rc",
        #                  "core/stats",
        #                  "group=job/" + str(jobId),
        #                  "--rc-addr=127.0.0.1:5572"],
        #                 check=True,
        #                 stdout=subprocess.PIPE,
        #                 stderr=subprocess.PIPE,)
        stats = requests.post("http://127.0.0.1:5572/core/stats", json={
            "group": "job/" + str(jobId)
        })
        stats.raise_for_status()