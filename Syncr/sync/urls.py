from django.urls import path
from . import views

urlpatterns = [
    path('', views.IndexView.as_view(), name='index'),
    
    path('ajax/runningjobs', views.ajaxRunningJobs.as_view(), name='runningJobs'),
    
    
    path('test/', views.testView.as_view(), name='test'),
]