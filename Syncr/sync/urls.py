from django.urls import path
from . import views

urlpatterns = [
    path('', views.IndexView.as_view(), name='index'),
    path('create/', views.createJobView.as_view(), name='create'),
    path('task/create/', views.createTaskView.as_view(), name='createTask'),
    path('detail/<int:jobId>', views.detailView.as_view(), name='detail'),
    
    path('ajax/runningjobs', views.ajaxRunningJobs.as_view(), name='runningJobs'),
    path('ajax/finishedjobs', views.ajaxFinishedJobs.as_view(), name='finishedJobs'),
    path('ajax/jobquery/<int:jobId>', views.ajaxJobQuery.as_view(), name='jobQuery'),
    
    
    path('test/', views.testView.as_view(), name='test'),
]