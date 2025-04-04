from django.urls import path
from django.contrib.auth.decorators import login_required
from . import views

app_name = "billing"

urlpatterns = [
    path('', login_required(views.IndexView.as_view()), name='index'),
]