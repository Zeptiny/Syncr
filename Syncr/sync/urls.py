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
    
    
    # Schedule
    path('schedule/', views.scheduleView.as_view(), name='schedule'),
    path('schedule/create/', views.createScheduleView.as_view(), name='createSchedule'),
    
    path('ajax/schedulelist', views.ajaxScheduleListView.as_view(), name='scheduleList'),
    
    
    # Remotes
    path('remote/', views.remoteView.as_view(), name='remote'),
    path('remote/create/', views.createRemoteView.as_view(), name='createRemote'),
    
    
    path('ajax/remote/list/', views.ajaxRemoteList.as_view(), name='remoteList'),
]