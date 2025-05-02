from django.urls import path
import os
from . import views

app_name = "accounts"

urlpatterns = [
    path('login/', views.loginView.as_view(), name='login'),
    path('logout/', views.logoutView.as_view(), name='logout'),
]

if os.environ.get("DJANGO_REGISTRATION_ENABLED"):
    urlpatterns += path('register/', views.registerView.as_view(), name='register')