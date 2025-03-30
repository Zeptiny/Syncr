from django.shortcuts import render, redirect
from django.views import View
from django.contrib.auth import authenticate, login, logout
from django.contrib import messages
from django.contrib.auth.forms import UserCreationForm
from django.contrib.auth.decorators import login_required

class loginView(View):
    def get(self, request):
        return render(request, 'accounts/login.html')

    def post(self, request):
        username = request.POST.get('username')
        password = request.POST.get('password')
        user = authenticate(request, username=username, password=password)
        if user is not None:
            login(request, user)
            return redirect('sync:index')
        else:
            messages.error(request, "Invalid username or password")
            return render(request, 'accounts/login.html')
        
class logoutView(View):
    def get(self, request):
        return render(request, 'accounts/logout.html')
    def post(self, request):
        logout(request)
        messages.info(request, "You have been logged out.")
        return redirect('accounts:login')
    
class registerView(View):
    def get(self, request):
        form = UserCreationForm()
        context = {
            'form': form,
        }
        return render(request, 'accounts/register.html', context)

    def post(self, request):
        form = UserCreationForm(request.POST)
        
        if form.is_valid():
            user = form.save()
            login(request, user)
            messages.success(request, "Registration successful.")
            return redirect('sync:index')
        
        else:
            messages.error(request, "Registration failed. Please try again.")
            context = {
                'form': form,
            }
            return render(request, 'accounts/register.html', context)