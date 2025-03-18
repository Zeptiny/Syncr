from django.urls import path
from . import views

app_name = "sync"

urlpatterns = [
    path('', views.IndexView.as_view(), name='index'),
    path('job/run/', views.createJobView.as_view(), name='runJob'),
    path('schedule/create/', views.createTaskView.as_view(), name='createTask'),
    
    path('remote/create/', views.createRemoteView.as_view(), name='createRemote'),
    path('detail/<int:jobId>', views.detailView.as_view(), name='detail'),
    
    path('ajax/runningjobs', views.ajaxRunningJobs.as_view(), name='runningJobs'),
    path('ajax/finishedjobs', views.ajaxFinishedJobs.as_view(), name='finishedJobs'),
    path('ajax/jobquery/<int:jobId>', views.ajaxJobQuery.as_view(), name='jobQuery'),
    
    
    path('test/', views.testView.as_view(), name='test'),
]