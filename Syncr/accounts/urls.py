from django.urls import path
from . import views

app_name = "accounts"

urlpatterns = [
    path('login/', views.loginView.as_view(), name='login'),
    path('logout/', views.logoutView.as_view(), name='logout'),
    path('register/', views.registerView.as_view(), name='register'),
]