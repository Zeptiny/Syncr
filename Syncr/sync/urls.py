from django.urls import path
from django.contrib.auth.decorators import login_required
from . import views

app_name = "sync"

urlpatterns = [
    path('', login_required(views.IndexView.as_view()), name='index'),
    path('ajax/indexStats', login_required(views.ajaxIndexStatsView.as_view()), name='indexStats'),
    path('ajax/indexStatsCharts', login_required(views.ajaxIndexStatsChartsView.as_view()), name='indexStatsCharts'),
    
    # Jobs
    path('job/run/', login_required(views.createJobView.as_view()), name='runJob'),
    path('job/<int:jobId>', login_required(views.detailView.as_view()), name='detail'),
    path('job/list/', login_required(views.jobListView.as_view()), name='jobList'),
    
    path('ajax/runningjobs', login_required(views.ajaxRunningJobs.as_view()), name='runningJobs'),
    path('ajax/finishedjobs', login_required(views.ajaxFinishedJobs.as_view()), name='finishedJobs'),
    path('ajax/jobquery/<int:jobId>', login_required(views.ajaxJobQuery.as_view()), name='jobQuery'),
    path('ajax/jobquerycharts/<int:jobId>', login_required(views.ajaxJobQueryCharts.as_view()), name='jobQueryCharts'),
    
    
    
    # Schedule
    path('schedule/', login_required(views.scheduleView.as_view()), name='schedule'),
    path('schedule/new/', login_required(views.createScheduleView.as_view()), name='createSchedule'),
    path('schedule/edit/<int:scheduleId>/', login_required(views.createScheduleView.as_view()), name='editSchedule'),
    path('schedule/delete/<int:scheduleId>/', login_required(views.deleteScheduleView.as_view()), name='deleteSchedule'),
    path('schedule/<int:scheduleId>/', login_required(views.scheduleDetailView.as_view()), name='scheduleDetail'),
    
    path('ajax/schedule/<int:scheduleId>/', login_required(views.scheduleDetailJobsView.as_view()), name='scheduleDetailJobs'),
    
    
    # Forms
    path('ajax/genericCopyOptionsForm/', login_required(views.ajaxGenericCopyOptionsForm.as_view()), name='genericCopyOptionsForm'),
    
    
    # Remotes
    path('remote/', login_required(views.remoteView.as_view()), name='remote'),
    path('remote/new/', login_required(views.createRemoteView.as_view()), name='createRemote'),
    path('remote/edit/<int:remoteId>', login_required(views.createRemoteView.as_view()), name='editRemote'),
    path('remote/delete/<int:remoteId>', login_required(views.deleteRemoteView.as_view()), name='deleteRemote'),
    
    path('union/new/', login_required(views.createUnionView.as_view()), name='createUnion'),
    path('union/edit/<int:unionId>', login_required(views.createUnionView.as_view()), name='editUnion'),
    path('union/delete/<int:unionId>', login_required(views.deleteUnionView.as_view()), name='deleteUnion'),
]