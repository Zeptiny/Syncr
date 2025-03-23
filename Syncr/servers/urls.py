from django.urls import path
from . import views

app_name = "servers"

urlpatterns = [
    path('list/', views.serverListView.as_view(), name='index'),
    path('<int:pk>/', views.serverDetailView.as_view(), name='detail')
]