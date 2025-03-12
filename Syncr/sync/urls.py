from django.urls import path
from . import views

urlpatterns = [
    path('', views.IndexView.as_view(), name='index'),
    
    path('test2/', views.test2Htmx.as_view(), name='test2'),
    path('test/', views.testHtmx.as_view(), name='test'),
]