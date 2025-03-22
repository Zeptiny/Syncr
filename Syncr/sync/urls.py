from django.urls import path
from . import views

app_name = "sync"

urlpatterns = [
    path('', views.IndexView.as_view(), name='index'),
    path('ajax/indexStats', views.ajaxIndexStatsView.as_view(), name='indexStats'),
    path('ajax/indexStatsCharts', views.ajaxIndexStatsChartsView.as_view(), name='indexStatsCharts'),
    
    # Jobs
    path('job/run/', views.createJobView.as_view(), name='runJob'),
    path('detail/<int:jobId>', views.detailView.as_view(), name='detail'),
    
    path('ajax/runningjobs', views.ajaxRunningJobs.as_view(), name='runningJobs'),
    path('ajax/finishedjobs', views.ajaxFinishedJobs.as_view(), name='finishedJobs'),
    path('ajax/jobquery/<int:jobId>', views.ajaxJobQuery.as_view(), name='jobQuery'),
    path('ajax/jobquerycharts/<int:jobId>', views.ajaxJobQueryCharts.as_view(), name='jobQueryCharts'),
    
    
    
    # Schedule
    path('schedule/', views.scheduleView.as_view(), name='schedule'),
    path('schedule/new/', views.createScheduleView.as_view(), name='createSchedule'),
    path('schedule/edit/<int:scheduleId>/', views.createScheduleView.as_view(), name='editSchedule'),
    path('schedule/delete/<int:scheduleId>/', views.deleteScheduleView.as_view(), name='deleteSchedule'),
    
    path('ajax/schedulelist', views.ajaxScheduleListView.as_view(), name='scheduleList'),
    
    
    # Remotes
    path('remote/', views.remoteView.as_view(), name='remote'),
    path('remote/new/', views.createRemoteView.as_view(), name='createRemote'),
    path('remote/edit/<int:remoteId>', views.createRemoteView.as_view(), name='editRemote'),
    path('remote/delete/<int:remoteId>', views.deleteRemoteView.as_view(), name='deleteRemote'),
    
    
    path('ajax/remote/list/', views.ajaxRemoteList.as_view(), name='remoteList'),
]