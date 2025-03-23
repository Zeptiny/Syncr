from django.urls import path
from django.contrib.admin.views.decorators import staff_member_required
from . import views

app_name = "servers"

urlpatterns = [
    path('list/', staff_member_required(views.serverListView.as_view()), name='list'),
    path('<int:pk>/', staff_member_required(views.serverDetailView.as_view()), name='detail')
]