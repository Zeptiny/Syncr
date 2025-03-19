import random
from django.http import HttpResponse
from time import sleep
from django.shortcuts import render, redirect
from django.views import View
from django.db.models import Sum
from django.utils import timezone
from datetime import timedelta

import subprocess
import threading
import requests

from . import utils
from . import models
from . import forms
from .settings import REMOTE_FIELDS

class IndexView(View):
    def get(self, request):
        return render(request, 'sync/index.html')
    
class ajaxIndexStatsView(View):
    def get(self, request):
        context = {
            'runningJobs': models.Job.objects.filter(finished=False, user=request.user).count(),
            'scheduledJobs': models.Task.objects.filter(user=request.user).count(),
            'erroredJobs': models.Job.objects.filter(success=False, user=request.user, startTime__gte=(timezone.now() - timedelta(days=30))).count(),
            'totalBandwidth': models.Job.objects.filter(user=request.user, startTime__gte=(timezone.now() - timedelta(days=30))).aggregate(totalBandwidth=Sum('bytes'))['totalBandwidth'] or 0
        }
        return render(request, 'sync/ajax/indexStats.html', context)

class createRemoteView(View):
    def get(self, request):
        form = forms.remoteCreateForm()
        
        context = {
            'form': form,
            'remote_fields': REMOTE_FIELDS
        }
        
        return render(request, 'sync/remoteCreate.html', context)
    
    def post(self, request):
        form = forms.remoteCreateForm(request.POST)
        
        if form.is_valid():
            remote = form.save(commit=False)
            config_data = {}

            # Capture dynamic fields based on the remote type
            remote_type = form.cleaned_data['type']
            fields = REMOTE_FIELDS.get(remote_type, [])
            for field in fields:
                config_data[field] = request.POST.get(field, "") # Defaults to nothing if the field is not present
            # Add more conditions for other remote types as needed

            # Store the config data as JSON
            remote.config = config_data
            remote.user = request.user
            remote.save()
            
            return redirect('index')
        
        else:
            context = {
                'form': form,
                'remote_fields': REMOTE_FIELDS
            }
            return render(request, 'sync/remoteCreate.html', context)

class createJobView(View):
    def get(self, request):
        form = forms.jobCreateForm(request=request)
        
        context = {
            'form': form
        }
        
        return render(request, 'sync/jobCreate.html', context)
    
    def post(self, request):
        form = forms.jobCreateForm(request.POST, request=request)
        
        if form.is_valid():
            # Get the form data
            type = form.cleaned_data['type']
            srcFs = form.cleaned_data['srcFs']
            dstFs = form.cleaned_data['dstFs']
            
            # Create the job
            utils.createJobHandler(type, srcFs, dstFs, request.user)
            
            return redirect('index')
        
        else:
            context = {
                'form': form
            }
            return render(request, 'sync/jobCreate.html', context)

class detailView(View):
    def get(self, request, jobId):
        job = models.Job.objects.get(pk=jobId)
        context = {
            'job': job
        }
        return render(request, 'sync/detail.html', context)
    
# Schedules
class scheduleView(View):
    def get(self, request):
        return render(request, 'sync/schedule.html')

class createTaskView(View):
    def get(self, request):
        form = forms.taskCreateForm(request=request)
        
        context = {
            'form': form
        }
        
        return render(request, 'sync/taskCreate.html', context)
    def post(self, request):
        form = forms.taskCreateForm(request.POST, request=request)
        
        if form.is_valid():
            task = form.save(commit=False)
            task.user = request.user
            
            task.save()
            
            return redirect('index')
        
        else:
            context = {
                'form': form
            }
            return render(request, 'sync/taskCreate.html', context)
        
class ajaxScheduleListView(View):
    def get(self, request):
        schedules = models.Task.objects.filter(user=request.user)
        context = {
            'schedules': schedules
        }
        
        return render(request, 'sync/ajax/scheduleList.html', context)

# Ajax Views Below


# We are not using utils.queryJob as we need the transferring and checking
class ajaxJobQuery(View):
    def get(self, request, jobId):
        
        # If the job is finished, reload the user page to query from the database
        job = models.Job.objects.get(pk=jobId)
        if job.finished:
            return redirect('detail', jobId=jobId)
        
        # If the job is not finished, query the job directly from rclone
        # We do not query from the database to transfer the load to the rclone server
        else:
            rcloneJobId = job.rcloneId
            
            status = requests.post("http://127.0.0.1:5572/job/status", json={
                "jobid": rcloneJobId
            })
            status.raise_for_status()
            stats = requests.post("http://127.0.0.1:5572/core/stats", json={
                "group": "job/" + str(rcloneJobId)
            })
            stats.raise_for_status()
            
            query = {
                **status.json(),
                **stats.json()
            }
            
            context = {
                'query': query
            }
            return render(request, 'sync/ajax/jobQuery.html', context)

        
class ajaxRunningJobs(View):
    def get(self, request):
        runningJobs = models.Job.objects.filter(finished=False, user=request.user).order_by('-startTime')
        context = {
            'runningJobs': runningJobs
        }
        
        return render(request, 'sync/ajax/runningJobs.html', context)

class ajaxFinishedJobs(View):
    def get(self, request):
        finishedJobs = models.Job.objects.filter(finished=True, user=request.user).order_by('-startTime')[:20] # Only get the last 20
        context = {
            'finishedJobs': finishedJobs
        }
        
        return render(request, 'sync/ajax/finishedJobs.html', context)







class testView(View):
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