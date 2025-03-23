import random
from django.http import HttpResponse
from time import sleep
from django.shortcuts import get_object_or_404, render, redirect
from django.views import View
from django.db.models import Sum
from django.utils import timezone
from datetime import timedelta

from django.core.paginator import Paginator
from django.db.models import Q
from urllib.parse import urlencode

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
            'scheduledJobs': models.Schedule.objects.filter(user=request.user).count(),
            'erroredJobs': models.Job.objects.filter(success=False, user=request.user, startTime__gte=(timezone.now() - timedelta(days=30))).count(),
            'totalBandwidth': models.Job.objects.filter(user=request.user, startTime__gte=(timezone.now() - timedelta(days=30))).aggregate(totalBandwidth=Sum('bytes'))['totalBandwidth'] or 0
        }
        return render(request, 'sync/ajax/indexStats.html', context)
    
class ajaxIndexStatsChartsView(View):
    def get(self, request):
        days = [timezone.now().date() - timedelta(days=7-i) for i in range(7)]
        
        stats = models.DailyStatistics.objects.filter(user=request.user, date__in=days)
        
        stats_by_date = {stat.date: stat for stat in stats}
        
        # Initialize the context dictionary
        context = {
            'days': [day.strftime("%m-%d") for day in days],
            'bytes': [],
            'serverSideCopyBytes': [],
            'serverSideMoveBytes': [],
            'jobsRun': [],
            'jobsErrored': []
        }
        
        # Prepare the data for the chart
        for day in days:
            if day in stats_by_date:
                context['bytes'].append((stats_by_date[day].bytes)/1000000000)
                context['serverSideCopyBytes'].append((stats_by_date[day].serverSideCopyBytes)/1000000000)
                context['serverSideMoveBytes'].append((stats_by_date[day].serverSideMoveBytes)/1000000000)
                context['jobsRun'].append(stats_by_date[day].jobs_run)
                context['jobsErrored'].append(stats_by_date[day].errored_jobs)
            else:
                context['bytes'].append(0)
                context['serverSideCopyBytes'].append(0)
                context['serverSideMoveBytes'].append(0)
                context['jobsRun'].append(0)
                context['jobsErrored'].append(0)
        
        return render(request, 'sync/ajax/indexStatsCharts.html', context)

# Remotes

class createRemoteView(View):
    def get(self, request, remoteId=None):
        
        if remoteId: # If there is a remote id
            if models.Remote.objects.filter(pk=remoteId).exists(): # If an object with that ID exists
                remote = models.Remote.objects.get(pk=remoteId)
                
                if remote.user != request.user: # Redirect to the normal creation if the user is not the owner
                    return redirect('sync:createRemote')
                
                form = forms.remoteCreateForm(instance=remote)
            else: # If it doesn't exist
                return redirect('sync:createRemote') # Redirect to without an ID
        else:
            form = forms.remoteCreateForm()
        
        context = {
            'form': form,
            'remote_fields': REMOTE_FIELDS
        }
        
        return render(request, 'sync/remoteCreate.html', context)
    
    def post(self, request, remoteId=None):
        if remoteId: # If there is a remote id
            if models.Remote.objects.filter(pk=remoteId).exists(): # If an object with that ID exists
                remote = models.Remote.objects.get(pk=remoteId)
                if remote.user != request.user: # Redirect to the normal creation if the user is not the owner
                    return redirect('sync:createRemote')

                form = forms.remoteCreateForm(request.POST, instance=remote)            
            else: # If it doesn't exist
                return redirect('sync:createRemote') # Redirect to without an ID
        else:
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
            
            return redirect('sync:remote')
        
        else:
            context = {
                'form': form,
                'remote_fields': REMOTE_FIELDS
            }
            return render(request, 'sync/remoteCreate.html', context)
        
class deleteRemoteView(View):
    def post(self, request, remoteId):
        remote = get_object_or_404(models.Remote, pk=remoteId)
        
        if remote.user == request.user:
            remote.delete()
        
        return redirect('sync:remote')

class remoteView(View):
    def get(self, request):
        return render(request, 'sync/remote.html')

class ajaxRemoteList(View):
    def get(self, request):
        remotes = models.Remote.objects.filter(user=request.user)
        context = {
            'remotes': remotes
        }
        
        return render(request, 'sync/ajax/remoteList.html', context)
# Jobs

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
            
            return redirect('sync:index')
        
        else:
            context = {
                'form': form
            }
            return render(request, 'sync/jobCreate.html', context)

class detailView(View):
    def get(self, request, jobId):
        isFinished = models.Job.objects.get(pk=jobId).finished
        context = {
            'jobId': jobId,
            'isFinished': isFinished
        }
        return render(request, 'sync/jobDetail.html', context)

class jobListView(View):
    def get(self, request):
        searchForm = forms.jobsSearchForm(request.GET, request=request)

        filters = Q(user=request.user)

        if searchForm.is_valid():
            search = searchForm.cleaned_data.get('search')
            job_type = searchForm.cleaned_data.get('type')
            callee = searchForm.cleaned_data.get('callee')
            status = searchForm.cleaned_data.get('status')
            last_x_days = searchForm.cleaned_data.get('last_x_days')

            if search:
                filters &= (
                    Q(id__icontains=search) | 
                    Q(srcFs__name__icontains=search) | 
                    Q(dstFs__name__icontains=search)
                )
                            
            if job_type:
                filters &= Q(type=job_type)
            if callee:
                filters &= Q(schedule__isnull=True) if callee == 'manual' else Q(schedule__name=callee)
            if status:
                status_filters = {
                    'success': Q(success=True, finished=True),
                    'failed': Q(success=False, finished=True),
                    'running': Q(finished=False),
                }
                filters &= status_filters.get(status, Q())  
            if last_x_days:
                days_ago = timezone.now() - timedelta(days=int(last_x_days))
                filters &= Q(startTime__gte=days_ago)

        jobs = models.Job.objects.filter(filters).order_by('-startTime')

        paginator = Paginator(jobs, 10)
        page_number = request.GET.get('page')
        jobs = paginator.get_page(page_number)
        
        # Build query string without 'page'
        query_params = request.GET.copy()
        query_params.pop('page', None)  # Remove existing page parameter
        query_string = urlencode(query_params)  # Encode remaining parameters
        
        context = {
            'jobs': jobs,
            'searchForm': searchForm,
            'query_string': query_string
        }

        return render(request, 'sync/jobList.html', context)

# We are not using utils.queryJob as we want the transferring and checking
class ajaxJobQuery(View):
    def get(self, request, jobId):
        job = models.Job.objects.get(pk=jobId)
        context = {
            'job': job
        }
        return render(request, 'sync/ajax/jobQuery.html', context)
    
class ajaxJobQueryCharts(View):
    def get(self, request, jobId):
        job = models.Job.objects.get(id=jobId)
        
        # Fetch all the statistics for the job
        # We ignore the first one as it will be all fucking zeros
        stats = models.jobRunStatistics.objects.filter(job=job).order_by('dateTime')[1:]
        
        relativeTimes = [(stat.dateTime - job.startTime).total_seconds() for stat in stats]  # Convert to minutes

        # Prepare the data for the chart
        context = {
            'times': relativeTimes,
            
            'bytes': [stat.speed / 1_000_000 for stat in stats],  # Convert bytes to megabytes
            'serverSideCopyBytes': [stat.speedServerSideCopy / 1_000_000 for stat in stats],  # Convert bytes to megabytes
            'serverSideMoveBytes': [stat.speedServerSideMove / 1_000_000 for stat in stats],  # Convert bytes to megabytes
            
            'transferSpeed': [stat.transferSpeed for stat in stats],
            'transferSpeedServerSideCopy': [stat.transferSpeedServerSideCopy for stat in stats],
            'transferSpeedServerSideMove': [stat.transferSpeedServerSideMove for stat in stats],
            
            'checks': [stat.checks for stat in stats]
        }
        
        return render(request, 'sync/ajax/jobQueryCharts.html', context)
    
# Schedules
class scheduleView(View):
    def get(self, request):
        return render(request, 'sync/schedule.html')

class createScheduleView(View):
    def get(self, request, scheduleId=None):
        
        if scheduleId: # If there is a schedule id
            if models.Schedule.objects.filter(pk=scheduleId).exists(): # If an object with that ID exists
                schedule = models.Schedule.objects.get(pk=scheduleId)
                
                if schedule.user != request.user: # Redirect to the normal creation if the user is not the owner
                    return redirect('sync:createSchedule')
                
                form = forms.scheduleCreateForm(instance=schedule, request=request)
            else: # If it doesn't exist
                return redirect('sync:createSchedule') # Redirect to without an ID
        else:
            form = forms.scheduleCreateForm(request=request)
        
        context = {
            'form': form
        }
        
        return render(request, 'sync/scheduleCreate.html', context)
    
    def post(self, request, scheduleId=None):
        if scheduleId: # If there is a schedule id
            if models.Schedule.objects.filter(pk=scheduleId).exists(): # If an object with that ID exists
                schedule = models.Schedule.objects.get(pk=scheduleId)
                if schedule.user != request.user: # Redirect to the normal creation if the user is not the owner
                    return redirect('sync:createSchedule')

                form = forms.scheduleCreateForm(request.POST, instance=schedule, request=request)            
            else: # If it doesn't exist
                return redirect('sync:createSchedule') # Redirect to without an ID
        else:
            form = forms.scheduleCreateForm(request.POST, request=request)
        
        if form.is_valid():
            schedule = form.save(commit=False)
            schedule.user = request.user
            
            schedule.save()
            
            return redirect('sync:schedule')
        
        else:
            context = {
                'form': form
            }
            return render(request, 'sync/scheduleCreate.html', context)

class deleteScheduleView(View):
    def post(self, request, scheduleId):
        schedule = get_object_or_404(models.Schedule, pk=scheduleId)
        
        if schedule.user == request.user:
            schedule.delete()
        
        return redirect('sync:schedule')
    
      
class ajaxScheduleListView(View):
    def get(self, request):
        schedules = models.Schedule.objects.filter(user=request.user)
        context = {
            'schedules': schedules
        }
        
        return render(request, 'sync/ajax/scheduleList.html', context)

# Ajax Views Below

        
class ajaxRunningJobs(View):
    def get(self, request):
        runningJobs = models.Job.objects.filter(finished=False, user=request.user).order_by('-startTime')
        context = {
            'runningJobs': runningJobs
        }
        
        return render(request, 'sync/ajax/indexJobRunning.html', context)

class ajaxFinishedJobs(View):
    def get(self, request):
        finishedJobs = models.Job.objects.filter(finished=True, user=request.user).order_by('-endTime')[:20] # Only get the last 20
        context = {
            'finishedJobs': finishedJobs
        }
        
        return render(request, 'sync/ajax/indexJobFinished.html', context)